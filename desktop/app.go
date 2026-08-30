package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	rt "vantaloom.local/apps/desktop/internal/runtime"
)

// bootEvent is the event name the splash frontend listens on for live progress.
const bootEvent = "vantaloom:boot"

// updatePromptEvent asks the splash to show the in-page update modal. We use an
// HTML modal (not wruntime.MessageDialog) because the native Windows dialog only
// offers yes/no and ignores custom button labels, so the user's choice was
// effectively lost ("选啥都没用").
const updatePromptEvent = "vantaloom:update-prompt"

// App is the Wails-bound application object. Its exported methods are callable
// from the splash UI as window.go.main.App.<Method>().
type App struct {
	ctx context.Context
	mgr *rt.Manager

	// updateAnswer carries the user's choice from the in-page update modal back
	// to a blocked Bootstrap. Guarded by updateMu (a fresh channel per prompt).
	updateMu     sync.Mutex
	updateAnswer chan bool

	// monitorOnce ensures the background health monitor starts at most once.
	monitorOnce sync.Once
	// lastVersion is the version observed after Bootstrap, used by the monitor.
	lastVersion string

	// ctlPort/ctlToken describe the local window-control HTTP endpoint (see
	// startCtl). Zero port = the ctl server failed to start.
	ctlPort  int
	ctlToken string

	// route 非空 = 副窗口模式（--route，已通过 validateSubWindowRoute 校验）：
	// Bootstrap 跳过安装/更新编排，只等待运行时可达后加载该站内路由。
	route string
}

// NewApp builds the App with a runtime Manager bound to the default install
// prefix (overridable by VANTALOOM_HOME) and the public npm registry.
// route 非空时进入副窗口模式（见 window_route.go）。
func NewApp(route string) *App {
	return &App{mgr: rt.New("", ""), route: route}
}

// startup is wired to Wails OnStartup; it captures the runtime context used for
// emitting events and for cancelling work when the window closes.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.startCtl()
}

// ── Window-control HTTP endpoint ─────────────────────────────────────────────
//
// The webview navigates away from the Wails asset origin to the local app
// (http://127.0.0.1:<apiPort>), where Wails does NOT inject window.go /
// window.runtime — they only exist on pages served by the Wails asset server.
// The in-app frameless title bar therefore drives the window over a tiny
// localhost HTTP endpoint instead: the shell appends ?vtlshell=1&vtlport=…&
// vtltoken=… to the app URL, and the frontend fetches /minimise etc. Window
// DRAG can't go over HTTP; the frontend posts the raw "drag" message on the
// native webview channel (chrome.webview.postMessage / webkit.messageHandlers.
// external), which is exactly how the official Wails runtime implements
// --wails-draggable under the hood.

// startCtl starts the loopback window-control server on a random port with a
// per-launch token. Best-effort: on failure the app still works, the in-app
// title bar just loses min/max/close (drag is independent of this server).
func (a *App) startCtl() {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		fmt.Printf("[desktop] ctl token: %v\n", err)
		return
	}
	token := hex.EncodeToString(buf)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("[desktop] ctl listen: %v\n", err)
		return
	}

	mux := http.NewServeMux()
	action := func(path string, fn func()) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			if r.URL.Query().Get("t") != token {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fn()
			w.WriteHeader(http.StatusNoContent)
		})
	}
	action("/minimise", func() { wruntime.WindowMinimise(a.ctx) })
	action("/toggle-maximise", func() { wruntime.WindowToggleMaximise(a.ctx) })
	action("/close", func() { wruntime.Quit(a.ctx) })
	// /drag and /resize hand the pointer interaction to the OS via
	// WM_NCLBUTTONDOWN (see winctl_windows.go) — native move/size loops with
	// Aero Snap. Windows-only; macOS drags via the webview "drag" message.
	action("/drag", func() { _ = startNativeDrag() })
	mux.HandleFunc("/resize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.URL.Query().Get("t") != token {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if err := startNativeResize(r.URL.Query().Get("edge")); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// /open-window 拉起一个新的壳进程实例作为副窗口（如 IDE 独立窗口）。
	// POST body {"route":"/ide/?machine=…"}；route 只允许站内相对路径（与
	// --route 同一套白名单校验，见 window_route.go）。成功 200 {"ok":true}，
	// 失败 4xx/5xx {"ok":false,"error":"中文原因"}。新进程与本进程互相独立。
	mux.HandleFunc("/open-window", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		// fetch 带 application/json 时浏览器会先发 CORS 预检；预检无副作用，
		// 直接放行（真正的调用仍需 token）。
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON := func(status int, ok bool, errMsg string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			payload := map[string]any{"ok": ok}
			if errMsg != "" {
				payload["error"] = errMsg
			}
			_ = json.NewEncoder(w).Encode(payload)
		}
		if r.URL.Query().Get("t") != token {
			writeJSON(http.StatusForbidden, false, "口令校验失败")
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(http.StatusMethodNotAllowed, false, "仅支持 POST")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
		if err != nil {
			writeJSON(http.StatusBadRequest, false, "读取请求体失败")
			return
		}
		var req struct {
			Route string `json:"route"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(http.StatusBadRequest, false, `请求体必须是 {"route":"…"} 形式的 JSON`)
			return
		}
		if _, err := validateSubWindowRoute(req.Route); err != nil {
			writeJSON(http.StatusBadRequest, false, err.Error())
			return
		}
		if err := spawnSubWindow(req.Route); err != nil {
			writeJSON(http.StatusInternalServerError, false, err.Error())
			return
		}
		writeJSON(http.StatusOK, true, "")
	})
	// /capture lets the in-app 截图 button work again (its old window.go path
	// was dead on the external origin for the same injection reason).
	mux.HandleFunc("/capture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.URL.Query().Get("t") != token {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		b64, err := captureMainWindow()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(b64))
	})

	a.ctlPort = ln.Addr().(*net.TCPAddr).Port
	a.ctlToken = token
	go func() { _ = http.Serve(ln, mux) }()

	// 调试钩子（默认关闭）：设置 VANTALOOM_CTL_INFO_FILE 时把本进程的 ctl
	// 端口与 token 写入该文件，供外部 e2e 测试驱动窗控端点。能给本进程设
	// 环境变量的人与进程本就同一信任域，不扩大攻击面。
	if p := os.Getenv("VANTALOOM_CTL_INFO_FILE"); p != "" {
		_ = os.WriteFile(p, []byte(fmt.Sprintf(`{"pid":%d,"port":%d,"token":"%s"}`+"\n", os.Getpid(), a.ctlPort, a.ctlToken)), 0o600)
	}
}

// appURL is the URL the webview loads after Bootstrap: the local app plus the
// desktop-shell markers the in-app title bar detects. 副窗口模式下目标是
// <基址><route>；route 可能自带查询串（如 /ide/?machine=…），此时壳参数必须
// 用 & 续接而不是再来一个 ?。
func (a *App) appURL() string {
	target := a.mgr.BackendURL()
	if a.route != "" {
		target = strings.TrimSuffix(target, "/") + a.route
	}
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	if a.ctlPort == 0 {
		return target + sep + "vtlshell=1"
	}
	return fmt.Sprintf("%s%svtlshell=1&vtlport=%d&vtltoken=%s", target, sep, a.ctlPort, a.ctlToken)
}

// GetStatus returns the current install/run snapshot (for diagnostics in the UI).
func (a *App) GetStatus() rt.Status {
	return a.mgr.Status(a.ctx)
}

// OpenInBrowser opens a URL in the user's default browser — a fallback offered
// on the splash if in-window navigation is undesired.
func (a *App) OpenInBrowser(url string) {
	wruntime.BrowserOpenURL(a.ctx, url)
}

// CaptureWebview returns a base64-encoded PNG of the app window so the frontend
// can offer a "snapshot the page" affordance now that the bundled Obscura
// headless browser engine can't itself produce screenshots.
//
// On Windows it BitBlts the window's client area from the screen DC (pure Win32
// syscalls — no cgo/deps), capturing the actual rendered WebView2 pixels
// including the cross-origin <iframe> preview. macOS/Linux report unsupported
// (see screenshot_other.go) and the frontend falls back (hides the snapshot
// button). Bound to the UI as window.go.main.App.CaptureWebview().
func (a *App) CaptureWebview() (string, error) {
	return captureMainWindow()
}

// Bootstrap detects the local runtime and brings it to a ready state, emitting
// progress events as it goes, then returns the URL the webview should load.
//
//	not installed         → install latest, start
//	installed, stopped    → auto-update if a newer version is reachable, then start
//	already running        → return immediately
//
// The whole flow is unprivileged except for the one-time legacy mesh-service
// cleanup (see rt.Manager.uninstallLegacyMeshOnce), which self-gates behind a
// done-marker and never blocks on a declined elevation prompt.
func (a *App) Bootstrap() (string, error) {
	// 副窗口（--route）走独立启动路径：主窗口已负责安装/更新/启动编排，这里
	// 只等待运行时可达 —— 两个进程绝不并发写安装目录。
	if a.route != "" {
		return a.bootstrapSubWindow()
	}

	emit := func(phase, msg string, pct int) {
		wruntime.EventsEmit(a.ctx, bootEvent, rt.Progress{Phase: phase, Message: msg, Percent: pct})
	}

	ctx, cancel := context.WithTimeout(a.ctx, 12*time.Minute)
	defer cancel()

	// Ensure a usable Node.js exists before booting the runtime: the launcher
	// scripts shell out to the system `node`. Auto-installs a
	// pinned LTS from a China mirror if absent. Non-fatal — boot proceeds with
	// whatever node may already be present on failure.
	nodeCtx, cancelNode := context.WithTimeout(ctx, 3*time.Minute)
	if _, err := rt.EnsureNode(nodeCtx); err != nil {
		fmt.Printf("[desktop] Node.js 检测/安装失败（继续启动）: %v\n", err)
	}
	cancelNode()

	st := a.mgr.Status(ctx)

	if st.Running {
		// Backend already up. We never silently hot-update a running backend (an
		// active agent session would be killed mid-run) — but we also don't
		// silently skip. If a newer version is reachable, ASK the user: they know
		// whether an agent is currently running. If one is, they defer to the next
		// open; if not, they can stop + update + restart right now.
		emit("check", "正在检查更新…", 1)
		checkCtx, cancelCheck := context.WithTimeout(ctx, 6*time.Second)
		available, latest, checkErr := a.mgr.UpdateAvailable(checkCtx)
		cancelCheck()

		if checkErr != nil || !available {
			// No update (or offline) — hand straight off to the live app.
			emit("ready", "后端已在运行", 100)
			a.lastVersion = st.RunningVersion
			url := a.appURL()
			a.startMonitor()
			return url, nil
		}

		// Ask via an in-page modal rendered by the splash (see updatePromptEvent).
		if !a.askUpdatePrompt(ctx, latest) {
			emit("ready", "后端已在运行（本次跳过更新）", 100)
			a.lastVersion = st.RunningVersion
			url := a.appURL()
			a.startMonitor()
			return url, nil
		}

		// User confirmed no agent is running: stop the runtime, then update.
		emit("update", "正在停止当前服务…", 2)
		if err := a.mgr.Stop(ctx); err != nil {
			// Can't stop cleanly — don't strand the user; hand off to the running app.
			emit("ready", "无法停止当前服务，已跳过本次更新", 100)
			a.lastVersion = st.RunningVersion
			url := a.appURL()
			a.startMonitor()
			return url, nil
		}
		emit("update", fmt.Sprintf("正在更新到 %s…", latest), 3)
		if _, err := a.mgr.Install(ctx, "latest", func(p rt.Progress) {
			wruntime.EventsEmit(a.ctx, bootEvent, p)
		}); err != nil {
			// Update failed after stopping — start what's already installed so the
			// user isn't left with a stopped backend.
			emit("start", "更新未完成，正在启动已安装版本…", 88)
			if e := a.mgr.Start(ctx); e != nil {
				return "", e
			}
		}
		// Install() starts the runtime; fall through to the shared health wait.
	} else {
		switch {
		case !st.Installed:
			emit("install", "首次运行：正在安装 Vantaloom 核心服务…", 0)
			if _, err := a.mgr.Install(ctx, "latest", func(p rt.Progress) {
				wruntime.EventsEmit(a.ctx, bootEvent, p)
			}); err != nil {
				return "", err
			}

		default:
			// Installed but stopped. Try an auto-update, but cap the registry check
			// so an offline launch falls back quickly to the installed version.
			emit("check", "正在检查更新…", 1)
			checkCtx, cancelCheck := context.WithTimeout(ctx, 6*time.Second)
			available, latest, checkErr := a.mgr.UpdateAvailable(checkCtx)
			cancelCheck()

			if checkErr == nil && available {
				emit("update", fmt.Sprintf("发现新版本 %s，正在更新…", latest), 2)
				if _, err := a.mgr.Install(ctx, "latest", func(p rt.Progress) {
					wruntime.EventsEmit(a.ctx, bootEvent, p)
				}); err != nil {
					// Update failed mid-flight — don't strand the user; start what's
					// already installed.
					emit("start", "更新未完成，正在启动已安装版本…", 88)
					if e := a.mgr.Start(ctx); e != nil {
						return "", e
					}
				}
			} else {
				emit("start", "正在启动后端服务…", 88)
				if err := a.mgr.Start(ctx); err != nil {
					return "", err
				}
			}
		}
	}

	emit("wait", "正在等待后端就绪…", 95)
	if err := a.mgr.WaitHealthy(ctx, 90*time.Second); err != nil {
		return "", err
	}

	// Record the running version for the health monitor.
	if v, ok := a.mgr.Health(a.ctx); ok {
		a.lastVersion = v
	}

	emit("ready", "就绪", 100)
	url := a.appURL()
	a.startMonitor()
	return url, nil
}

// bootstrapSubWindow 是副窗口（--route）的启动路径：完全跳过 Node 检测与
// 运行时的安装/更新/启动编排（那些是主窗口的职责），只探测运行时是否可达，
// 可达即加载 <基址><route>。等待超时返回明确的中文错误 —— splash 会显示错误
// 与「重试」按钮，不会静默转圈。
func (a *App) bootstrapSubWindow() (string, error) {
	emit := func(phase, msg string, pct int) {
		wruntime.EventsEmit(a.ctx, bootEvent, rt.Progress{Phase: phase, Message: msg, Percent: pct})
	}

	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()

	emit("check", "正在检测本地运行时…", 5)
	st := a.mgr.Status(ctx)
	if !st.Running && !st.Installed {
		return "", fmt.Errorf("未检测到本地运行时：请先打开 Vantaloom 主窗口完成安装并启动服务，再打开此窗口")
	}

	emit("wait", "正在等待后端服务就绪…", 40)
	if err := a.mgr.WaitHealthy(ctx, 45*time.Second); err != nil {
		return "", fmt.Errorf("后端服务未就绪（等待超时）：请确认 Vantaloom 主窗口或本地运行时已启动，然后点击重试")
	}

	// 记录运行版本，供健康监视器在后端更新重启后自动刷新页面。
	if v, ok := a.mgr.Health(a.ctx); ok {
		a.lastVersion = v
	}

	emit("ready", "就绪", 100)
	url := a.appURL()
	a.startMonitor()
	return url, nil
}

// startMonitor launches the background health monitor (at most once).
func (a *App) startMonitor() {
	a.monitorOnce.Do(func() {
		go a.monitorBackend()
	})
}

// autoReviveAfter is how long the backend must stay unreachable before the
// monitor tries to start it again itself. The backend is NOT supervised (the
// Windows tray that used to respawn it was removed — see vantaloomctl's
// componentSpecs), so a crashed vantaloom-api stays dead until something
// restarts it. Before this existed the only cure was closing and reopening the
// window, which is exactly what users reported.
const autoReviveAfter = 8 * time.Second

// monitorBackend polls the backend health endpoint. When the backend restarts
// with a different version (e.g. after a CLI-driven update), it force-reloads
// the webview so the user gets the new frontend without restarting the shell.
// When the backend goes down it shows a recovery overlay, tries to revive the
// backend, and tears the overlay back down once it answers again.
//
// 三条不变量（都是修 bug 修出来的，别退回去）：
//
//  1. **覆盖层必须显式拆掉。** 它挂在宿主页的 document.body 上，而恢复动作走
//     __vtlReloadApp——那只刷 iframe，不动宿主页。于是「加」有代码、「减」没有，
//     一旦弹出就永久糊在那儿，关窗重开是唯一出路。removeOverlayJS 是那个减号。
//  2. **文案不许替后端编故事。** 壳看不见真正的更新：前端的「检查更新」spawn 的
//     是一个脱离进程的 updater，壳只看得到「后端没了」。所以以前对**任何**健康
//     中断都写「正在更新 Vantaloom」，而绝大多数中断其实是后端崩了——用户盯着一
//     个根本不存在的更新进度条。现在只陈述可观测的事实。
//  3. **必须有人真的去救。** 后端无人监管，光显示进度条不会让它回来。
func (a *App) monitorBackend() {
	downSince := time.Time{}
	overlayShown := false
	revived := false

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}

		v, ok := a.mgr.Health(a.ctx)
		if !ok {
			if downSince.IsZero() {
				downSince = time.Now()
			}
			down := time.Since(downSince)
			// Show an overlay after the backend has been down for 4+ seconds
			// (ignore brief healthcheck blips during normal operation).
			if !overlayShown && down > 4*time.Second {
				overlayShown = true
				wruntime.WindowExecJS(a.ctx, downOverlayJS)
			}
			// Try to bring it back exactly once per outage. `vantaloomctl start`
			// is idempotent: if the process is actually alive and merely wedged,
			// it won't double-start it.
			if !revived && down > autoReviveAfter {
				revived = true
				go func() {
					if err := a.mgr.Start(a.ctx); err != nil {
						fmt.Printf("[desktop] auto-revive backend: %v\n", err)
					}
				}()
			}
			continue
		}

		// Backend is up.
		if overlayShown {
			overlayShown = false
			// Tear the overlay down FIRST: the reload below only refreshes the
			// app iframe, which cannot remove a node owned by the host page.
			wruntime.WindowExecJS(a.ctx, removeOverlayJS)
			// Backend came back — force reload to pick up new frontend.
			wruntime.WindowExecJS(a.ctx, "window.__vtlReloadApp ? window.__vtlReloadApp() : window.location.reload()")
			a.lastVersion = v
			downSince = time.Time{}
			revived = false
			continue
		}

		if a.lastVersion != "" && v != "" && v != a.lastVersion {
			// Version changed without downtime (hot restart). Reload.
			a.lastVersion = v
			wruntime.WindowExecJS(a.ctx, "window.__vtlReloadApp ? window.__vtlReloadApp() : window.location.reload()")
		}
		downSince = time.Time{}
		revived = false
	}
}

// RestartBackend restarts the local runtime. Bound to the recovery overlay's
// 「重启后端」 button as window.go.main.App.RestartBackend() — the overlay is
// injected into the HOST page, which is served from the Wails asset origin, so
// the window.go bindings are available to it.
func (a *App) RestartBackend() {
	go func() {
		if err := a.mgr.Start(a.ctx); err != nil {
			fmt.Printf("[desktop] manual restart backend: %v\n", err)
		}
	}()
}

// DismissOverlay removes the recovery overlay unconditionally. Bound to the
// overlay's 「关闭提示」 button: the last-resort escape hatch so a user is never
// again trapped behind a full-screen panel with no way out but killing the
// window.
func (a *App) DismissOverlay() {
	wruntime.WindowExecJS(a.ctx, removeOverlayJS)
}

// overlayID is the host-page node id for the backend-down overlay. Both the
// inject and the remove script key off it — they are a matched pair; changing
// one without the other is how the overlay became unremovable in the first
// place.
const overlayID = "__vt_update_overlay"

// removeOverlayJS tears the overlay back down. It exists because the overlay
// lives on the HOST page while every recovery path (__vtlReloadApp) only
// refreshes the app iframe, so nothing on the reload path could ever remove it.
const removeOverlayJS = `(function(){
  var d=document.getElementById('` + overlayID + `');
  if(d && d.parentNode) d.parentNode.removeChild(d);
})();`

// downOverlayJS injects a full-screen overlay when the backend stops answering,
// matching the Vantaloom splash aesthetic.
//
// 它只陈述壳真正知道的事：后端没响应、正在自动重连。**不写「正在更新」**——壳
// 看不见更新（前端触发的 updater 是脱离进程的），而后端中断绝大多数是崩溃，写
// 「正在更新」就是对着用户编一个不存在的进度。三颗按钮是硬性逃生口：在此之前
// 这个面板一旦弹出就没有任何出路。
const downOverlayJS = `(function(){
  if(document.getElementById('` + overlayID + `')) return;
  var d=document.createElement('div');
  d.id='` + overlayID + `';
  d.style.cssText='position:fixed;inset:0;z-index:99999;display:flex;align-items:center;justify-content:center;flex-direction:column;background:rgba(11,13,20,0.92);backdrop-filter:blur(12px);font-family:-apple-system,BlinkMacSystemFont,Segoe UI,PingFang SC,Microsoft YaHei,system-ui,sans-serif;color:#f3f5fb;';
  d.innerHTML='<div style="width:48px;height:48px;border-radius:14px;background:linear-gradient(135deg,#3ad0c8,#8b7bf0);display:flex;align-items:center;justify-content:center;font-size:24px;font-weight:700;color:#0b0d14;margin-bottom:18px">V</div><div style="font-size:15px;font-weight:600;margin-bottom:8px">后端服务未响应</div><div style="font-size:13px;color:#8b93a7;margin-bottom:20px;text-align:center;line-height:1.7;max-width:340px">正在自动重连并尝试重启后端，恢复后本页会自动刷新。<br>如果你刚触发了更新，这是正常的。</div><div style="width:180px;height:4px;border-radius:99px;background:rgba(255,255,255,0.08);overflow:hidden"><div style="width:35%;height:100%;border-radius:99px;background:linear-gradient(90deg,#3ad0c8,#8b7bf0);animation:__vt_slide 1.1s ease-in-out infinite"></div></div><div id="__vt_overlay_actions" style="display:none;gap:8px;margin-top:22px"><button id="__vt_btn_restart" style="background:rgba(255,255,255,0.07);border:1px solid rgba(255,255,255,0.14);color:#f3f5fb;font-size:12px;padding:7px 14px;border-radius:8px;cursor:pointer;font-family:inherit">重启后端</button><button id="__vt_btn_reload" style="background:rgba(255,255,255,0.07);border:1px solid rgba(255,255,255,0.14);color:#f3f5fb;font-size:12px;padding:7px 14px;border-radius:8px;cursor:pointer;font-family:inherit">刷新界面</button><button id="__vt_btn_dismiss" style="background:rgba(255,255,255,0.07);border:1px solid rgba(255,255,255,0.14);color:#f3f5fb;font-size:12px;padding:7px 14px;border-radius:8px;cursor:pointer;font-family:inherit">关闭提示</button></div><style>@keyframes __vt_slide{0%{margin-left:-35%}100%{margin-left:100%}}</style>';
  var api=(window.go&&window.go.main&&window.go.main.App)||null;
  d.querySelector('#__vt_btn_restart').onclick=function(){ if(api&&api.RestartBackend) api.RestartBackend(); };
  d.querySelector('#__vt_btn_reload').onclick=function(){
    if(window.__vtlReloadApp) window.__vtlReloadApp(); else window.location.reload();
  };
  d.querySelector('#__vt_btn_dismiss').onclick=function(){ if(d.parentNode) d.parentNode.removeChild(d); };
  // 按钮延后 10s 露出：短暂中断（更新重启）本来就会自愈，一上来就摆三颗
  // 按钮反而像出了大事。真卡住的才需要逃生口。
  setTimeout(function(){
    var acts=d.querySelector('#__vt_overlay_actions');
    if(acts) acts.style.display='flex';
  },10000);
  document.body.appendChild(d);
})();`

// askUpdatePrompt asks the splash to render the in-page update modal and blocks
// until the user answers via AnswerUpdatePrompt — true = stop+update+restart,
// false = skip this launch. A done context or a generous timeout defaults to
// skipping so a dismissed/stuck modal never strands the user.
func (a *App) askUpdatePrompt(ctx context.Context, latest string) bool {
	ch := make(chan bool, 1)
	a.updateMu.Lock()
	a.updateAnswer = ch
	a.updateMu.Unlock()
	defer func() {
		a.updateMu.Lock()
		a.updateAnswer = nil
		a.updateMu.Unlock()
	}()

	wruntime.EventsEmit(a.ctx, updatePromptEvent, map[string]any{"latest": latest})

	select {
	case proceed := <-ch:
		return proceed
	case <-ctx.Done():
		return false
	case <-time.After(10 * time.Minute):
		return false
	}
}

// ── Window controls (frameless title bar) ───────────────────────────────────

// Minimise minimises the window. Bound as window.go.main.App.Minimise().
func (a *App) Minimise() { wruntime.WindowMinimise(a.ctx) }

// ToggleMaximise toggles maximise/restore. Bound as window.go.main.App.ToggleMaximise().
func (a *App) ToggleMaximise() { wruntime.WindowToggleMaximise(a.ctx) }

// CloseWindow closes the app. Bound as window.go.main.App.CloseWindow().
func (a *App) CloseWindow() { wruntime.Quit(a.ctx) }

// AnswerUpdatePrompt is invoked from the splash modal with the user's choice:
// true = 结束并更新 (stop + update + restart now), false = 忽略本次更新 (skip).
// Bound to the UI as window.go.main.App.AnswerUpdatePrompt(bool).
func (a *App) AnswerUpdatePrompt(proceed bool) {
	a.updateMu.Lock()
	ch := a.updateAnswer
	a.updateMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- proceed:
	default:
	}
}
