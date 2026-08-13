// Vantaloom 桌面壳（Wails v2，单窗口）。
//
// ── 多窗口 / IDE 独立窗口（每窗口一个壳进程实例）──────────────────────────
//
// Wails v2 不支持多窗口，多窗口 = 多进程：
//
//   - 启动参数 `--route <站内路径>`（如 `--route "/ide/?machine=local&project=p1"`）
//     进入「副窗口模式」：跳过运行时的安装/更新/Node 编排（主窗口负责），只等
//     待运行时可达（超时给明确中文错误 + 重试按钮，不静默转圈），然后加载
//     `<运行时基址><route>` 并照旧追加 vtlshell=1&vtlport=&vtltoken=（route 已带
//     ? 时用 & 续接）。route 以 /ide 开头时窗口标题为「Vantaloom IDE」且默认
//     尺寸更大。route 非法（不以 / 开头、含协议/主机/反斜杠/控制字符）时忽略
//     并退回默认主窗口行为 —— 绝不把任意 URL 变成可加载目标。
//
//   - ctl 端点 `POST /open-window?t=<token>`，body {"route":"/ide/?…"}：对 route
//     做同一套站内白名单校验后，spawn 本进程自身可执行文件带 --route 拉起新
//     窗口。成功 200 {"ok":true}，失败 4xx/5xx {"ok":false,"error":"中文原因"}。
//     父子进程互相独立，任一方关闭不影响另一方。
//
//   - 每个进程自己 startCtl（随机端口 + 独立 token），窗控端点（/minimise、
//     /toggle-maximise、/close、/drag、/resize、/capture）在副进程原样可用；
//     Windows 下窗口定位按 PID 枚举（见 screenshot_windows.go findMainWindow），
//     不按标题查找 —— 多实例下按标题会命中别的进程的窗口。
//
//   - 单实例锁：壳自身没有任何单实例锁（未用 Wails SingleInstanceLock，也没有
//     运行时目录锁），多实例天然可行；副窗口跳过 Bootstrap 的安装/更新编排，
//     所以也不存在两个进程并发写安装目录的问题。
package main

import (
	"embed"
	"io/fs"
	"log"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assetsFS embed.FS

func main() {
	// The app UI runs inside a cross-origin <iframe> hosted by the Wails asset
	// page (the webview never leaves the Wails origin — that's where the
	// runtime's frameless drag/resize provably works). Chromium partitions
	// third-party iframe storage by default, which would give the embedded app
	// a DIFFERENT localStorage than it had as a top-level page (logins/prefs
	// gone). Disable partitioning so the iframe keeps the app's first-party
	// storage. WebView2's loader reads this env var at creation time.
	const storageFlag = "--disable-features=ThirdPartyStoragePartitioning"
	if cur := os.Getenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"); cur != "" {
		os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", cur+" "+storageFlag)
	} else {
		os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", storageFlag)
	}
	// Serve the splash from frontend/dist with index.html at the FS root.
	assets, err := fs.Sub(assetsFS, "frontend/dist")
	if err != nil {
		log.Fatalf("embed assets: %v", err)
	}

	// --route: 副窗口模式（IDE 独立窗口等）。非法 route 记日志并退回默认
	// 主窗口行为 —— 这是安全边界：绝不把任意输入变成可加载目标。
	route := ""
	if raw := parseRouteArg(os.Args[1:]); raw != "" {
		if v, rerr := validateSubWindowRoute(raw); rerr != nil {
			log.Printf("[desktop] 非法 --route，退回主窗口模式: %v", rerr)
		} else {
			route = v
		}
	}

	title := "Vantaloom"
	width, height := 1200, 820
	if isIDERoute(route) {
		// IDE 形态窗口：可区分标题 + 默认尺寸大一档。
		title = "Vantaloom IDE"
		width, height = 1520, 940
	}

	app := NewApp(route)

	err = wails.Run(&options.App{
		Title:            title,
		Width:            width,
		Height:           height,
		MinWidth:         480,
		MinHeight:        600,
		Frameless:        true,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 11, G: 13, B: 20, A: 255},
		OnStartup:        app.startup,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		log.Fatalf("vantaloom desktop: %v", err)
	}
}
