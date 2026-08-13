package main

// 副窗口（IDE 独立窗口）支持 —— 每个窗口 = 一个独立的壳进程实例。
//
// Wails v2 不支持多窗口，所以「再开一个窗口」的唯一路径是再拉起一个本可执行
// 文件的进程：新进程带 `--route <站内路径>` 启动，跳过安装/更新编排，只等待
// 运行时可达后加载 `<运行时基址><route>`。本文件集中放 route 的解析/校验与
// 子进程拉起逻辑，供 main.go（启动参数）与 app.go（ctl /open-window）共用。
//
// 安全边界：route 只允许「站内相对路径」。谁能起进程谁就能指定 route，但绝
// 不能借此把窗口指向外部站点 —— 因此拒绝协议/主机/协议相对（//）/反斜杠/
// 控制字符，校验不过就退回默认主窗口行为（启动参数路径）或报错（ctl 路径）。

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// parseRouteArg 从命令行参数提取 --route 的原始值，支持 "--route <值>" 与
// "--route=<值>" 两种写法；未提供时返回 ""。
func parseRouteArg(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--route" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "--route=") {
			return strings.TrimPrefix(a, "--route=")
		}
	}
	return ""
}

// validateSubWindowRoute 校验副窗口 route，只放行站内相对路径。
// 返回清理后的 route；任何越界形态（外部主机、协议、控制字符）都拒绝。
func validateSubWindowRoute(raw string) (string, error) {
	route := strings.TrimSpace(raw)
	if route == "" {
		return "", errors.New("route 不能为空")
	}
	if len(route) > 2048 {
		return "", errors.New("route 过长（上限 2048 字符）")
	}
	for _, r := range route {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("route 不允许包含控制字符或换行")
		}
	}
	// 浏览器会把路径中的反斜杠当正斜杠解析，"/\\evil.com" 等价于 "//evil.com"
	// （协议相对 = 外部主机），所以反斜杠一律拒绝。
	if strings.Contains(route, "\\") {
		return "", errors.New("route 不允许包含反斜杠")
	}
	if !strings.HasPrefix(route, "/") {
		return "", errors.New("route 必须是以 / 开头的站内相对路径")
	}
	if strings.HasPrefix(route, "//") {
		return "", errors.New("route 不允许以 // 开头（会被解析为外部主机）")
	}
	if strings.Contains(route, "://") {
		return "", errors.New("route 不允许携带协议或主机")
	}
	u, err := url.Parse(route)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return "", errors.New("route 解析失败或携带了协议/主机")
	}
	return route, nil
}

// isIDERoute 判断 route 是否指向 IDE 形态页面（/ide 前缀），用于窗口标题与
// 默认尺寸的区分。刻意用精确边界匹配，避免 "/identity" 之类误判。
func isIDERoute(route string) bool {
	return route == "/ide" ||
		strings.HasPrefix(route, "/ide/") ||
		strings.HasPrefix(route, "/ide?")
}

// spawnSubWindow 以 `--route <route>` 拉起本进程自身的可执行文件作为副窗口。
// 子进程与父进程互相独立：不共享生命周期、不建立 job/进程组耦合，父窗口关闭
// 不连带杀死子窗口，反之亦然（goroutine 里的 Wait 只负责回收进程句柄）。
func spawnSubWindow(route string) error {
	validated, err := validateSubWindowRoute(route)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件失败: %v", err)
	}
	cmd := exec.Command(exe, "--route", validated)
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动新窗口进程失败: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
