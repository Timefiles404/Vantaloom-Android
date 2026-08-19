package loomnet

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
)

const (
	// alpn is the QUIC/TLS application protocol; TLS 1.3 is forced (§3.1).
	alpn = "loom/1"

	idleTimeout        = 30 * time.Second
	keepAlivePeriod    = 15 * time.Second
	handshakeIdle      = 10 * time.Second
	maxIncomingStreams = 1 << 16
)

// transport owns the single shared UDP socket and its quic.Transport (§3.1):
// LAN-direct dialing and the inbound listener multiplex over this one socket.
type transport struct {
	acceptCtx        context.Context // node lifetime; bounds inbound accepts and sessions
	identity         *Identity
	dir              Directory
	allowProvisional func() bool // tempconn 配对窗口开启时接纳未知对端（见 verify.go）
	udp              *net.UDPConn
	qt               *quic.Transport
	quicConf         *quic.Config
}

func newTransport(acceptCtx context.Context, id *Identity, dir Directory, udpPort int, allowProvisional func() bool) (*transport, error) {
	// udpPort 0 = ephemeral (LAN-only machines re-advertise each heartbeat);
	// a fixed port is required for 公网直连 so port-forward/安全组 rules hold.
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: udpPort})
	if err != nil {
		return nil, fmt.Errorf("loomnet: bind overlay udp socket (port %d): %w", udpPort, err)
	}
	return &transport{
		acceptCtx:        acceptCtx,
		identity:         id,
		dir:              dir,
		allowProvisional: allowProvisional,
		udp:              udp,
		qt:               &quic.Transport{Conn: udp},
		quicConf: &quic.Config{
			MaxIdleTimeout:       idleTimeout,
			KeepAlivePeriod:      keepAlivePeriod,
			HandshakeIdleTimeout: handshakeIdle,
			MaxIncomingStreams:   maxIncomingStreams,
		},
	}, nil
}

func (t *transport) localUDPAddr() *net.UDPAddr {
	return t.udp.LocalAddr().(*net.UDPAddr)
}

// udpConn 返回 overlay 共享 UDP socket，供打洞逻辑复用（STUN 探测 + 打洞包
// 都从这个 socket 发出，保证 NAT 映射与 QUIC 连接复用同一端口）。
func (t *transport) udpConn() *net.UDPConn {
	return t.udp
}

// clientTLS is the outbound (dialer) config: it presents our certificate and
// pins the expected peer fingerprint (§2.3).
func (t *transport) clientTLS(expectedFingerprint string) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{t.identity.TLSCertificate()},
		NextProtos:            []string{alpn},
		MinVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyOutbound(expectedFingerprint),
	}
}

// serverTLS is the inbound (listener) config: it forces client certificates and
// accepts any peer whose CN→fingerprint is in the account set (§2.3), plus —
// when a tempconn pairing window is open — an unknown peer as a provisional
// (redeem-only) connection (see verifyInboundWithProvisional).
func (t *transport) serverTLS() *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{t.identity.TLSCertificate()},
		NextProtos:            []string{alpn},
		MinVersion:            tls.VersionTLS13,
		ClientAuth:            tls.RequireAnyClientCert,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyInboundWithProvisional(t.dir.AccountFingerprints, t.allowProvisional),
	}
}

// dial performs one outbound QUIC handshake to a concrete UDP address, pinning
// expectedFingerprint. intendedID is only for error context; the session's
// RemoteMachineID is taken from the verified peer certificate.
func (t *transport) dial(ctx context.Context, addr net.Addr, expectedFingerprint, intendedID string) (*quicSession, error) {
	conn, err := t.qt.Dial(ctx, addr, t.clientTLS(expectedFingerprint), t.quicConf)
	if err != nil {
		return nil, fmt.Errorf("loomnet: dial %s at %s: %w", intendedID, addr, err)
	}
	gotID, gotFP, err := peerIdentity(conn.ConnectionState().TLS)
	if err != nil {
		_ = conn.CloseWithError(quic.ApplicationErrorCode(1), "missing identity")
		return nil, err
	}
	return newQUICSession(t.acceptCtx, conn, gotID, gotFP), nil
}

func (t *transport) close() {
	if t.qt != nil {
		_ = t.qt.Close()
	}
	if t.udp != nil {
		_ = t.udp.Close()
	}
}

// dialQUICOnTransport 在给定的 quic.Transport（绑独立 UDP socket，如打洞 socket）
// 上拨一个 QUIC+mTLS 连接到 addr，pin expectedFingerprint。intendedID 仅用于错误
// 上下文。用于 P2P 打洞：打洞成功后在独立 socket 上建 QUIC 连接。
func dialQUICOnTransport(ctx context.Context, qt *quic.Transport, conn *net.UDPConn, addr *net.UDPAddr, expectedFingerprint, intendedID string, id *Identity) (*quicSession, error) {
	tlsConf := &tls.Config{
		Certificates:          []tls.Certificate{id.TLSCertificate()},
		NextProtos:            []string{alpn},
		MinVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyOutbound(expectedFingerprint),
	}
	qconn, err := qt.Dial(ctx, addr, tlsConf, &quic.Config{
		MaxIdleTimeout:       idleTimeout,
		KeepAlivePeriod:      keepAlivePeriod,
		HandshakeIdleTimeout: handshakeIdle,
		MaxIncomingStreams:   maxIncomingStreams,
	})
	if err != nil {
		return nil, fmt.Errorf("loomnet: punch dial %s at %s: %w", intendedID, addr, err)
	}
	gotID, fp, err := peerIdentity(qconn.ConnectionState().TLS)
	if err != nil {
		_ = qconn.CloseWithError(quic.ApplicationErrorCode(1), "missing identity")
		return nil, err
	}
	// quicSession 的 acceptCtx 用 context.Background()——独立 socket 的 QUIC
	// 连接不绑定 node 生命周期，由 session 自己管理。
	return newQUICSession(context.Background(), qconn, gotID, fp), nil
}

// tlsConfForServer 构造打洞 B 侧（被叫方）的 TLS server 配置：要求客户端证书，
// pin 期望的 peer 指纹（只接受这一个 peer 的连接）。用于 punch acceptPunchQUIC。
func tlsConfForServer(id *Identity, expectedPeerFingerprint string) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{id.TLSCertificate()},
		NextProtos:            []string{alpn},
		MinVersion:            tls.VersionTLS13,
		ClientAuth:            tls.RequireAnyClientCert,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyOutbound(expectedPeerFingerprint),
	}
}

// quicListener adapts inbound QUIC connections into a net.Listener that yields
// one net.Conn per accepted stream (§3.3), so vantaloom-api can serve overlay
// requests with the same mux as local requests. Each yielded conn carries the
// peer's mTLS-verified machineID for X-Loom-From stamping. onConn (0.14.3
// 反向互通) is invoked for every verified inbound connection so the node can
// adopt it as a reusable outbound session to that peer.
type quicListener struct {
	tr       *transport
	ln       *quic.Listener
	ctx      context.Context
	accepted chan net.Conn
	closed   chan struct{}
	once     sync.Once
	onConn   func(*quicSession)

	// demuxed guards against double-demuxing one QUIC connection（0.14.3 双向
	// 复用后同一条连接可能同时经 入站 accept 与 出站 storeConn 两条路径请求
	// demux——两个 AcceptStream 循环会把流瓜分到两个 goroutine，功能上无害但
	// 浪费；这里让后到者直接成为 no-op）。
	demuxMu sync.Mutex
	demuxed map[*quic.Conn]struct{}
}

func (t *transport) listen(onConn func(*quicSession)) (*quicListener, error) {
	ln, err := t.qt.Listen(t.serverTLS(), t.quicConf)
	if err != nil {
		return nil, fmt.Errorf("loomnet: start overlay listener: %w", err)
	}
	l := &quicListener{
		tr:       t,
		ln:       ln,
		ctx:      t.acceptCtx,
		accepted: make(chan net.Conn),
		closed:   make(chan struct{}),
		onConn:   onConn,
		demuxed:  map[*quic.Conn]struct{}{},
	}
	go l.acceptConns()
	return l, nil
}

func (l *quicListener) acceptConns() {
	for {
		conn, err := l.ln.Accept(l.ctx)
		if err != nil {
			return // listener closed or context done
		}
		id, fp, err := peerIdentity(conn.ConnectionState().TLS)
		if err != nil {
			_ = conn.CloseWithError(quic.ApplicationErrorCode(1), "missing identity")
			continue
		}
		if l.onConn != nil {
			l.onConn(newQUICSession(l.ctx, conn, id, fp))
		}
		go l.demux(conn, id, fp)
	}
}

// demux turns every stream a peer opens on one QUIC connection into a separate
// net.Conn handed to Accept, so http.Server treats each stream as its own HTTP
// connection. Idempotent per connection (see demuxed).
func (l *quicListener) demux(conn *quic.Conn, id, fp string) {
	l.demuxMu.Lock()
	if _, dup := l.demuxed[conn]; dup {
		l.demuxMu.Unlock()
		return
	}
	l.demuxed[conn] = struct{}{}
	l.demuxMu.Unlock()
	defer func() {
		l.demuxMu.Lock()
		delete(l.demuxed, conn)
		l.demuxMu.Unlock()
	}()
	for {
		st, err := conn.AcceptStream(l.ctx)
		if err != nil {
			return // connection gone
		}
		c := newStreamConn(conn, st, id, fp)
		select {
		case l.accepted <- c:
		case <-l.closed:
			_ = st.Close()
			return
		case <-l.ctx.Done():
			_ = st.Close()
			return
		}
	}
}

func (l *quicListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accepted:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (l *quicListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		_ = l.ln.Close()
	})
	return nil
}

func (l *quicListener) Addr() net.Addr { return l.tr.localUDPAddr() }
