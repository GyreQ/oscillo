// ai-trace 桌面客户端（Wails v3 壳）。
//
// 启动即拉起网关 + MITM + Dashboard 后端（若 8787 已有 serve 在跑则复用），
// 主窗口加载本机 Dashboard；菜单栏托盘常驻：打开窗口 / Capture 开关 /
// 数据目录 / 退出。UI 与浏览器版同一套（internal/web/static），
// ?desktop=1 让前端启用桌面专属控件（窗口拖拽区、Capture 按钮）。
package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"ai-trace/internal/analytics"
	"ai-trace/internal/capture"
	"ai-trace/internal/collector"
	"ai-trace/internal/gateway"
	"ai-trace/internal/live"
	"ai-trace/internal/mitm"
	"ai-trace/internal/store"
	"ai-trace/internal/web"
)

var version = "dev"

const (
	gatewayAddr  = "127.0.0.1:8787"
	mitmAddr     = "127.0.0.1:8788"
	dashboardURL = "http://" + gatewayAddr + "/?desktop=1"
)

//go:embed build/trayicon.png
var trayIconPNG []byte

var app *application.App

func main() {
	startBackend()
	applyTakeover()

	app = application.New(application.Options{
		Name:        "ai-trace",
		Description: "本地 AI Agent 追踪",
		// macOS 由 Finder 保证单实例（Wails 的 SingleInstance 走 session bus，
		// 在 mac 上不可用——magpie 同样只在 Windows/Linux 启用）
		SingleInstance: singleInstanceOptions(),
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyRegular,
		},
		ErrorHandler: func(err error) { log.Println("ai-trace:", err) },
		// 所有正常退出路径（Cmd+Q / 系统终止）都先还原接管的外部配置
		ShouldQuit: func() bool {
			runCleanup()
			return true
		},
	})

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		runCleanup()
		os.Exit(0)
	}()

	makeTray()
	makeWindow()

	app.Run()
}

// ---- 后端：复刻 serveCmd（常驻部署同款：config.json + 网关 + MITM + Dashboard）----

// backendConfig ~/.ai-trace/config.json（与 CLI serve 共用）。
type backendConfig struct {
	Upstreams    map[string]string `json:"upstreams"`
	Proxy        string            `json:"proxy"`
	CaptureHosts []string          `json:"captureHosts"`
}

var defaultCaptureHosts = []string{
	"anthropic.com", "openai.com", "generativelanguage.googleapis.com",
	"openrouter.ai", "deepseek.com", "moonshot.ai", "moonshot.cn",
	"zhipu.ai", "bigmodel.cn", "together.ai",
	"copilot.tencent.com", "workbuddy.cn", "codebuddy.cn",
}

func dataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ai-trace"
	}
	return filepath.Join(home, ".ai-trace")
}

// startBackend 若 8787 已有服务（用户 nohup serve 常驻）则复用；
// 否则在进程内启动网关 + MITM + Dashboard。返回是否由本进程启动。
func startBackend() bool {
	if probeHTTP("http://" + gatewayAddr + "/healthz") {
		return false
	}
	st, err := store.Open(dataDir())
	if err != nil {
		log.Println("打开数据目录失败:", err)
		return false
	}
	ups := map[string]string{}
	proxyUp := ""
	captureHosts := defaultCaptureHosts
	if cfg, err := loadBackendConfig(filepath.Join(dataDir(), "config.json")); err == nil {
		for k, v := range cfg.Upstreams {
			ups[k] = v
		}
		proxyUp = cfg.Proxy
		if cfg.CaptureHosts != nil {
			captureHosts = cfg.CaptureHosts
		}
	}
	if v := os.Getenv("AI_TRACE_UPSTREAM_PROXY"); v != "" {
		proxyUp = v
	}
	if v := os.Getenv("AI_TRACE_UPSTREAM_ANTHROPIC"); v != "" {
		ups["anthropic"] = v
	}
	if v := os.Getenv("AI_TRACE_UPSTREAM_OPENAI"); v != "" {
		ups["openai-chat"] = v
		ups["openai-responses"] = v
	}
	hub := live.NewHub()
	gw := gateway.New(gateway.Options{Addr: gatewayAddr, Upstreams: ups, Hub: hub, Proxy: proxyUp}, st)
	col := collector.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/v1/", gw.Handler())
	mux.Handle("/v1beta/", gw.Handler())
	mux.Handle("/", web.Handler(st, col, analytics.LoadPriceTable(dataDir()), hub, dataDir()))

	// MITM 代理（另一端口）：解 TLS 后按 Host 转发的网关实例
	if ca, caErr := mitm.LoadCA(filepath.Join(dataDir(), "ca")); caErr == nil {
		mitmGW := gateway.New(gateway.Options{Addr: mitmAddr, HostUpstream: true, Hub: hub, Proxy: proxyUp}, st)
		proxy := mitm.NewProxy(mitm.ProxyOptions{Addr: mitmAddr, CA: ca,
			Handler: mitmGW.Handler(), DecryptHosts: captureHosts, OutboundProxy: proxyUp})
		go func() {
			if err := proxy.ListenAndServe(); err != nil {
				log.Println("MITM 代理退出:", err)
			}
		}()
	} else {
		log.Println("CA 初始化失败，MITM 未启动:", caErr)
	}

	go func() {
		if err := http.ListenAndServe(gatewayAddr, mux); err != nil {
			log.Println("监听失败:", err)
		}
	}()
	// 等网关就绪再开窗口（页面立即有数据）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if probeHTTP("http://" + gatewayAddr + "/healthz") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func loadBackendConfig(path string) (*backendConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c backendConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func probeHTTP(url string) bool {
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// ---- 窗口与托盘 ----

var mainWin *application.WebviewWindow

// singleInstanceOptions 仅 Windows / Linux（macOS 由 Finder 保证单实例）。
func singleInstanceOptions() *application.SingleInstanceOptions {
	if runtime.GOOS == "darwin" {
		return nil
	}
	return &application.SingleInstanceOptions{
		UniqueID: "dev.ai-trace.desktop",
		OnSecondInstanceLaunch: func(application.SecondInstanceData) {
			showMain()
		},
	}
}

func makeWindow() {
	mainWin = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "main",
		Title:  "Oscillo",
		URL:    dashboardURL,
		Width:  1080,
		Height: 720,
		MinWidth:  860,
		MinHeight: 560,
		Mac: application.MacWindow{
			// 标题栏藏进页面：红绿灯悬浮于页面顶栏上（前端顶栏留出左侧空间）
			TitleBar: application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(0xf4, 0xf4, 0xf6),
	})
	// 关窗 = 隐藏（托盘常驻继续录制）；退出走托盘菜单
	mainWin.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if mainWin != nil {
			mainWin.Hide()
		}
		e.Cancel()
	})
}

func showMain() {
	// 事件回调（托盘 / 菜单 / 二次启动）都在主线程触发，直接操作窗口
	if mainWin != nil {
		mainWin.Show()
		mainWin.Focus()
	}
}

// makeTray 菜单栏托盘。
func makeTray() {
	menu := app.NewMenu()
	menu.Add("打开 Oscillo").OnClick(func(*application.Context) { showMain() })
	menu.AddSeparator()

	// Capture 开关（标签随状态切换）
	capItem := menu.Add("Capture：开启系统代理").OnClick(func(*application.Context) {
		go toggleCapture()
	})
	menu.AddSeparator()
	menu.Add("打开数据目录").OnClick(func(*application.Context) { openPath(dataDir()) })
	menu.Add("在浏览器中打开").OnClick(func(*application.Context) { openPath("http://127.0.0.1:8787") })
	menu.AddSeparator()
	menu.Add("退出 Oscillo").OnClick(func(*application.Context) {
		runCleanup()
		app.Quit()
	})
	refreshCapLabel := func() {
		on := capture.Read().On
		label := "Capture：开启系统代理"
		if on {
			label = "Capture：关闭并还原系统代理"
		}
		capItem.SetLabel(label)
	}
	refreshCapLabel()

	tray := app.SystemTray.New()
	tray.SetTooltip("Oscillo — 本地 AI Agent 追踪")
	tray.SetTemplateIcon(trayIconPNG)
	tray.SetMenu(menu)
	tray.OnClick(func() { showMain() })

	// 页面里的 Capture 按钮改了状态后，轮询刷新托盘标签。
	// 等 app 启动完成再开始（InvokeAsync 需要主线程 loop 已在跑）。
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go func() {
			last := capture.Read().On
			for {
				time.Sleep(2 * time.Second)
				on := capture.Read().On
				if on != last {
					last = on
					application.InvokeAsync(refreshCapLabel)
				}
			}
		}()
	})
}

// ---- 退出还原 / 启动恢复：ai-trace 接管的外部配置 ----
//
// ai-trace 可能接管两类外部状态：系统代理（capture on）和 WorkBuddy 的
// 应用内代理（settings.json 的 http.proxy）。退出时全部还原、并把这些
// 接管记入 takeover.json；下次启动读到该文件就恢复到退出前的状态——
// 保证「ai-trace 关掉后 WorkBuddy 等应用依然可用」，同时重启不丢监控。

const mitmProxyURL = "http://127.0.0.1:8788"

// takeoverState 退出时记录的接管状态。
type takeoverState struct {
	CaptureOn   bool `json:"captureOn"`
	WorkbuddyOn bool `json:"workbuddyOn"`
}

var cleanupOnce sync.Once

// runCleanup 幂等退出清理（托盘退出 / ShouldQuit / SIGTERM 三路共用）。
func runCleanup() { cleanupOnce.Do(cleanupTakeover) }

func workbuddySettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".workbuddy", "settings.json")
}

// injectWorkbuddyProxy 把 WorkBuddy 代理指向 MITM（首次改动前备份原文件）。
func injectWorkbuddyProxy() {
	p := workbuddySettingsPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return // 未安装 WorkBuddy，静默跳过
	}
	var cfg map[string]any
	if json.Unmarshal(raw, &cfg) != nil {
		log.Println("[takeover] workbuddy settings.json 解析失败，跳过")
		return
	}
	if cur, _ := cfg["http.proxy"].(string); cur == mitmProxyURL {
		return // 已处于接管状态
	}
	bak := p + ".bak-ai-trace-proxy"
	if _, err := os.Stat(bak); os.IsNotExist(err) {
		if err := os.WriteFile(bak, raw, 0o644); err != nil {
			log.Println("[takeover] workbuddy 备份失败，不改动:", err)
			return
		}
	}
	cfg["http.proxy"] = mitmProxyURL
	cfg["http.proxySupport"] = "override" // 等价于其设置界面选「手动代理」
	if out, jerr := json.Marshal(cfg); jerr == nil {
		if err := os.WriteFile(p, out, 0o644); err != nil {
			log.Println("[takeover] workbuddy settings.json 写入失败:", err)
		}
	}
}

// restoreWorkbuddyProxy 从备份还原 WorkBuddy 的代理设置。
func restoreWorkbuddyProxy() {
	p := workbuddySettingsPath()
	if p == "" {
		return
	}
	bak := p + ".bak-ai-trace-proxy"
	raw, err := os.ReadFile(bak)
	if err != nil {
		return // 没有接管过
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		log.Println("[takeover] workbuddy 还原失败:", err)
		return
	}
	os.Remove(bak)
}

// workbuddyTakeoverActive 备份存在即视为接管中。
func workbuddyTakeoverActive() bool {
	p := workbuddySettingsPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p + ".bak-ai-trace-proxy")
	return err == nil
}

// applyTakeover 启动时恢复上次退出前的接管状态（takeover.json 不存在
// 说明上次是干净退出或首次运行，不动任何东西）。崩溃残留的接管无需
// 恢复：系统代理仍指 8788 而本进程已重新监听，自愈。
func applyTakeover() {
	raw, err := os.ReadFile(filepath.Join(dataDir(), "takeover.json"))
	if err != nil {
		return
	}
	var st takeoverState
	if json.Unmarshal(raw, &st) != nil {
		return
	}
	if st.CaptureOn {
		if _, err := capture.On(dataDir()); err != nil {
			log.Println("[takeover] 恢复 capture 失败:", err)
		}
	}
	if st.WorkbuddyOn {
		injectWorkbuddyProxy()
	}
	os.Remove(filepath.Join(dataDir(), "takeover.json"))
}

// cleanupTakeover 退出前：记录接管状态 → 还原系统代理 → 还原 WorkBuddy。
func cleanupTakeover() {
	st := takeoverState{
		CaptureOn:   capture.Read().On,
		WorkbuddyOn: workbuddyTakeoverActive(),
	}
	if out, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(filepath.Join(dataDir(), "takeover.json"), out, 0o644)
	}
	if st.CaptureOn {
		if _, err := capture.Off(dataDir()); err != nil {
			log.Println("[takeover] 退出还原系统代理失败:", err)
		}
	}
	restoreWorkbuddyProxy()
}

// toggleCapture 托盘菜单的 Capture 开关（与 UI 按钮同一逻辑）。
func toggleCapture() {
	if capture.Read().On {
		if _, err := capture.Off(dataDir()); err != nil {
			log.Println("capture off 失败:", err)
		}
		return
	}
	if _, err := capture.On(dataDir()); err != nil {
		log.Println("capture on 失败:", err)
	}
}

func openPath(p string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", p)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", p)
	default:
		cmd = exec.Command("xdg-open", p)
	}
	cmd.Start()
}
