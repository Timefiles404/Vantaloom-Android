package loomnet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// relayYamuxConfig 返回中继电路上 yamux 会话的配置。默认 ConnectionWriteTimeout
// 只有 10s——公网中继链路的 UDP 抖动可能让 keepalive ping 写超时，导致 session
// 被误判死亡（日志 "keepalive failed: i/o deadline reached"、随后控制连接重建）。
// 调大到 30s 让短暂抖动（<30s）不触发误判；真实断网仍由连接级读错误快速驱动
// 恢复（serveIncoming 读失败立即 reset 重建），不会延迟断线感知。
func relayYamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.ConnectionWriteTimeout = 30 * time.Second
	return cfg
}

// relaySession 是中继电路上的 Session 实现：在中继字节管道上跑 mTLS（client
// 模式，pin 对端指纹）+ yamux 多路复用，得到与直连 quicSession 相同的
// OpenStream/AcceptStream 抽象。中继只见密文，A↔B 之间端到端加密。
type relaySession struct {
	ym       *yamux.Session
	remoteID string
	// fingerprint 是对端经 mTLS 证明的 SPKI 指纹。必须一路带到入站流上：
	// serveHandler 每个请求都要用 (CN, 指纹) 重算信任，拿不到指纹就一律当作
	// 「未受信」，于是除兑换端点外整条中继链路恒 403——中继连得上、却一个请求
	// 也过不去（0.15.37 发现的既有缺陷；relaySession 建立时明明已经验证过身份，
	// 只是没把结论传下去）。
	fingerprint string
}

// newRelaySession 在已配对的中继字节管道上完成 mTLS 握手（client 模式，pin
// 对端指纹）并建立 yamux 多路复用会话。pipe 是中继配对后的字节管道。ctx 控制
// 握手超时——必须传带 deadline 的 ctx（A 侧传 relayDialer 的 dctx），不能传
// context.Background()：否则握手卡住时（网络黑洞/对端恶意）依赖 QUIC idle
// timeout（30s）间接超时，relayTimeout=15s 形同虚设、B 侧 goroutine 被挂起。
func newRelaySession(ctx context.Context, pipe net.Conn, expectedFingerprint, remoteID string, id *Identity) (*relaySession, error) {
	tlsConf := &tls.Config{
		Certificates:          []tls.Certificate{id.TLSCertificate()},
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyOutbound(expectedFingerprint),
		MinVersion:            tls.VersionTLS13,
	}
	tlsConn := tls.Client(pipe, tlsConf)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		pipe.Close()
		return nil, fmt.Errorf("loomnet/relay: mTLS 握手失败: %w", err)
	}
	ym, err := yamux.Client(tlsConn, relayYamuxConfig())
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("loomnet/relay: 建立 yamux 会话: %w", err)
	}
	return &relaySession{ym: ym, remoteID: remoteID, fingerprint: expectedFingerprint}, nil
}

// newRelaySessionServer 在中继字节管道上完成 mTLS 握手（server 模式，pin 对端
// 指纹）并建立 yamux 多路复用会话（server 端）。用于 B 侧（被叫方）收到 INCOMING
// 并 ACCEPT 后的入站连接。pipe 是 ACCEPT 流配对后的字节管道。ctx 控制握手超时
// （B 侧在 handleIncoming goroutine 里，必须传带 deadline 的 ctx，见
// newRelaySession 注释）。
func newRelaySessionServer(ctx context.Context, pipe net.Conn, expectedFingerprint, remoteID string, id *Identity) (*relaySession, error) {
	return relayServerHandshake(ctx, pipe, id, verifyOutbound(expectedFingerprint), remoteID)
}

// newRelaySessionServerProvisional 是 B 侧接受**匿名会合**电路时的握手（tempconn）：
// 对端还没进信任表，所以既不能预先钉扎它的指纹，也不能相信它自报的 machineID。
// 校验改用与直连监听器同一个 verifyInboundWithProvisional——已知 CN 必须指纹相符，
// 未知 CN 仅在配对窗口开着时放行——身份则一律从握手完成后的证书里读，绝不采信
// 中继转发过来的 from 字段。得到的会话进 serveHandler 后仍按信任集逐请求重算：
// 未受信 = 只能够到兑换端点。
func newRelaySessionServerProvisional(ctx context.Context, pipe net.Conn, id *Identity, dir Directory, allowProvisional func() bool) (*relaySession, error) {
	var accountFingerprints func() map[string]string
	if dir != nil {
		accountFingerprints = dir.AccountFingerprints
	}
	return relayServerHandshake(ctx, pipe, id, verifyInboundWithProvisional(accountFingerprints, allowProvisional), "")
}

// relayServerHandshake 跑 server 侧 mTLS + yamux。verify 是证书校验回调；
// remoteID 为空时从证书 CN 取（会合模式：对端身份只能来自证书）。
func relayServerHandshake(ctx context.Context, pipe net.Conn, id *Identity, verify func([][]byte, [][]*x509.Certificate) error, remoteID string) (*relaySession, error) {
	tlsConf := &tls.Config{
		Certificates:       []tls.Certificate{id.TLSCertificate()},
		InsecureSkipVerify: true,
		// ClientAuth 必须显式要求客户端证书：server 侧的 VerifyPeerCertificate
		// 只有在真的收到证书时才会被调用，默认的 NoClientCert 之下它一次都不会
		// 执行——中继电路上的「mTLS」因此形同虚设，任何配对上来的对端都能被当成
		// 它自称的那台机器（0.15.37 发现；直连监听器 serverTLS 一直是对的，
		// 中继这条路漏了）。
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verify,
		MinVersion:            tls.VersionTLS13,
	}
	tlsConn := tls.Server(pipe, tlsConf)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		pipe.Close()
		return nil, fmt.Errorf("loomnet/relay: 入站 mTLS 握手失败: %w", err)
	}
	// 身份恒取自已验证的证书，而不是任何一方自报的字段。
	cn, fp, err := peerIdentity(tlsConn.ConnectionState())
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("loomnet/relay: 读取入站对端身份: %w", err)
	}
	if remoteID == "" {
		remoteID = cn
	}
	ym, err := yamux.Server(tlsConn, relayYamuxConfig())
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("loomnet/relay: 建立 yamux server 会话: %w", err)
	}
	return &relaySession{ym: ym, remoteID: remoteID, fingerprint: fp}, nil
}

// OpenStream 在中继电路上开一条新的 yamux 多路复用流（近端发送）。
// yamux 的 OpenStream 不接受 ctx，这里先检查 ctx 是否已取消。
func (s *relaySession) OpenStream(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := s.ym.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("loomnet/relay: open stream to %s: %w", s.remoteID, err)
	}
	return st, nil
}

// AcceptStream 接受对端打开的下一条 yamux 多路复用流（远端接收）。
func (s *relaySession) AcceptStream() (net.Conn, error) {
	st, err := s.ym.AcceptStream()
	if err != nil {
		return nil, fmt.Errorf("loomnet/relay: accept stream from %s: %w", s.remoteID, err)
	}
	return st, nil
}

// RemoteMachineID 是对端 mTLS 验证过的 machineID。
func (s *relaySession) RemoteMachineID() string { return s.remoteID }

// RemoteFingerprint 是对端 mTLS 验证过的 SPKI 指纹（serveHandler 逐请求重算
// 信任要用；见 relaySession.fingerprint 的注释）。
func (s *relaySession) RemoteFingerprint() string { return s.fingerprint }

// Close 关闭 yamux 会话（同时关闭底层 TLS 连接和中继字节管道）。yamux 的
// Close 是幂等的（第二次调用返回 nil），且会关闭 CloseChan 通知所有等待者。
func (s *relaySession) Close() error { return s.ym.Close() }

// CloseChan 返回会话死亡通知通道。通道在会话关闭时被关闭——无论是主动
// Close，还是底层中继电路死亡（网络黑洞、relay 静默掐断）触发 yamux
// recv/send 循环退出 → exitErr → Close。Node.watchConn 靠它感知中继会话
// 死亡并及时把死会话逐出缓存；否则死会话滞留缓存，后续请求拿它 OpenStream
// 会被阻塞最多 ConnectionWriteTimeout=30s（写超时）才报错，整个 HTTP 请求
// 卡死（QUIC 的 OpenStreamSync 快速失败，无此问题）。
func (s *relaySession) CloseChan() <-chan struct{} { return s.ym.CloseChan() }

// relayInboundListener 把 relaySession 的 yamux 流适配成 net.Listener，让
// httpSrv.Serve 能处理中继入站流。每个流包装成 relayStreamConn（携带
// RemoteMachineID 供 connContext 提取 peerID）。中继流不走 QUIC listener 的
// demux，需要这个独立适配器。
type relayInboundListener struct {
	sess     *relaySession
	accepted chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

// newRelayInboundListener 创建一个中继入站 listener 并启动 acceptLoop。
func newRelayInboundListener(s *relaySession) *relayInboundListener {
	l := &relayInboundListener{
		sess:     s,
		accepted: make(chan net.Conn, 16),
		closed:   make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

// acceptLoop 持续从 relaySession 接受 yamux 流，包装成 relayStreamConn 投递
// 给 accepted 通道。session 结束时关闭 accepted 通知 Serve 退出。
func (l *relayInboundListener) acceptLoop() {
	for {
		c, err := l.sess.AcceptStream()
		if err != nil {
			close(l.accepted) // 通知 Serve 结束
			return
		}
		rc := &relayStreamConn{Conn: c, remoteID: l.sess.RemoteMachineID(), fingerprint: l.sess.RemoteFingerprint()}
		select {
		case l.accepted <- rc:
		case <-l.closed:
			c.Close()
			return
		}
	}
}

// Accept 返回下一条中继入站流（已包装为 relayStreamConn）。
func (l *relayInboundListener) Accept() (net.Conn, error) {
	c, ok := <-l.accepted
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}

// Close 关闭 listener 和底层 relaySession。
func (l *relayInboundListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		l.sess.Close()
	})
	return nil
}

// Addr 返回一个占位地址（中继流无真实本地地址）。
func (l *relayInboundListener) Addr() net.Addr { return noopAddr{} }

// relayStreamConn 包装 yamux 流，携带 remoteID 供 connContext 提取 peerID。
// connContext 通过 interface{ RemoteMachineID() string } 类型断言提取，因此
// relayStreamConn 与 QUIC 的 streamConn 走同一路径。
type relayStreamConn struct {
	net.Conn
	remoteID    string
	fingerprint string
}

// RemoteMachineID 是对端 mTLS 验证过的 machineID（整个 session 共享一个）。
func (c *relayStreamConn) RemoteMachineID() string { return c.remoteID }

// RemoteFingerprint 是对端 mTLS 验证过的 SPKI 指纹（同上，整个 session 共享）。
// connContext 靠它把指纹送进请求上下文——缺了它 serveHandler 一律判「未受信」。
func (c *relayStreamConn) RemoteFingerprint() string { return c.fingerprint }

// noopAddr 是一个无操作的 net.Addr 实现，用于没有真实地址的 listener。
type noopAddr struct{}

func (noopAddr) Network() string { return "loomnet-relay" }
func (noopAddr) String() string  { return "loomnet-relay" }
