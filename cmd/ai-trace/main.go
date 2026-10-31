// ai-trace：本地 AI Agent 追踪工具。
//
//	ai-trace serve              启动网关 + Dashboard（阻塞）
//	ai-trace run <agent> [...]  注入 base_url 后启动 agent（自动拉起网关）
//	ai-trace run -- <cmd> [...] 通用模式：注入全部已知 base_url 变量
//	ai-trace open               打开 Dashboard（自动拉起网关）
//	ai-trace adapters           列出内置与已安装的 agent
//	ai-trace version            版本信息
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"ai-trace/internal/adapter"
	"ai-trace/internal/analytics"
	"ai-trace/internal/capture"
	"ai-trace/internal/collector"
	"ai-trace/internal/gateway"
	"ai-trace/internal/live"
	"ai-trace/internal/mitm"
	"ai-trace/internal/run"
	"ai-trace/internal/skills"
	"ai-trace/internal/store"
	"ai-trace/internal/web"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "serve":
		code = serveCmd(os.Args[2:])
	case "run":
		code = runCmd(os.Args[2:])
	case "open":
		code = openCmd()
	case "capture":
		code = captureCmd(os.Args[2:])
	case "doctor":
		code = doctorCmd()
	case "export":
		code = exportCmd(os.Args[2:])
	case "skills-backfill":
		code = skillsBackfillCmd()
	case "adapters":
		code = adaptersCmd()
	case "version", "--version", "-v":
		fmt.Printf("ai-trace %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Println(`ai-trace — 本地 AI Agent 追踪

用法:
  ai-trace serve               启动网关 + MITM + Dashboard
  ai-trace run <agent> [...]   追踪启动一个 agent（如 ai-trace run claude）
  ai-trace run -- <cmd> [...]  追踪任意命令（注入全部 base_url 变量）
  ai-trace open                打开 Dashboard
  ai-trace capture on|off      开/关系统代理（MITM 模式，抓桌面端等）
  ai-trace doctor              诊断本机 agent 并给出接入建议
  ai-trace adapters            列出支持的 agent
  ai-trace skills-backfill     从历史录制流量回填技能沉淀
数据目录: ~/.ai-trace`)
}

// dataDir 数据目录。
func dataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ai-trace"
	}
	return filepath.Join(home, ".ai-trace")
}

// serveConfig ~/.ai-trace/config.json：常驻部署的上游等配置。
type serveConfig struct {
	Upstreams map[string]string `json:"upstreams"`  // 协议 -> 上游 base url（anthropic / openai-chat / openai-responses）
	Proxy     string            `json:"proxy"`      // 上级代理（二级代理：MITM 解密后经它出网，如 http://127.0.0.1:7897）
	CaptureHosts []string       `json:"captureHosts"` // MITM 解密域名后缀；缺失/null = 内置默认，[] = 全部解密
}

// defaultCaptureHosts 内置解密列表：常见 LLM API + WorkBuddy/CodeBuddy 自家端点。
// 不在列表上的域名走隧道直通（不解密、零影响），直通时会打日志便于发现漏网域名。
var defaultCaptureHosts = []string{
	"anthropic.com", "openai.com", "generativelanguage.googleapis.com",
	"openrouter.ai", "deepseek.com", "moonshot.ai", "moonshot.cn",
	"zhipu.ai", "bigmodel.cn", "together.ai",
	"copilot.tencent.com", "workbuddy.cn", "codebuddy.cn",
	"hunyuan.tencent.com", "tencentcloudapi.com", // 腾讯混元系（WorkBuddy 官方模型后端）
}

// loadServeConfig 读配置文件；文件不存在返回 not exist 错误（调用方静默）。
func loadServeConfig(path string) (*serveConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c serveConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// serveCmd 启动网关 + Dashboard。
func serveCmd(args []string) int {
	addr := "127.0.0.1:8787"
	for i := 0; i < len(args); i++ {
		if args[i] == "--addr" && i+1 < len(args) {
			addr = args[i+1]
			i++
		}
	}
	st, err := store.Open(dataDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据目录失败: %v\n", err)
		return 1
	}
	// 上游覆盖。优先级：环境变量 > ~/.ai-trace/config.json > 协议默认值。
	// 配置文件让常驻模式不需要每次带 env（例：上游串 magpie）。
	ups := map[string]string{}
	var proxyUp string // 上级代理（二级代理）
	captureHosts := defaultCaptureHosts
	if cfg, err := loadServeConfig(filepath.Join(dataDir(), "config.json")); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[ai-trace] 读取配置文件失败（忽略）: %v\n", err)
		}
	} else {
		for k, v := range cfg.Upstreams {
			ups[k] = v
		}
		proxyUp = cfg.Proxy
		if cfg.CaptureHosts != nil {
			captureHosts = cfg.CaptureHosts // 显式配置（含空 = 全部解密）
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
	// 实时事件总线：两个网关共用，Dashboard 经 /api/live 订阅
	hub := live.NewHub()
	gw := gateway.New(gateway.Options{Addr: addr, Upstreams: ups, Hub: hub, Proxy: proxyUp}, st)
	col := collector.New()

	// 组合路由：网关（/v1、/v1beta）+ Dashboard（/、/api）
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/v1/", gw.Handler())
	mux.Handle("/v1beta/", gw.Handler())
	mux.Handle("/", web.Handler(st, col, analytics.LoadPriceTable(dataDir()), hub, dataDir()))

	// MITM 代理（同一进程，另一端口）：解 TLS 后按 Host 转发的网关实例
	ca, caErr := mitm.LoadCA(filepath.Join(dataDir(), "ca"))
	if caErr == nil {
		mitmGW := gateway.New(gateway.Options{Addr: mitmAddr, HostUpstream: true, Hub: hub, Proxy: proxyUp}, st)
		proxy := mitm.NewProxy(mitm.ProxyOptions{Addr: mitmAddr, CA: ca,
			Handler: mitmGW.Handler(), DecryptHosts: captureHosts, OutboundProxy: proxyUp})
		go func() {
			if err := proxy.ListenAndServe(); err != nil {
				fmt.Fprintf(os.Stderr, "[ai-trace] MITM 代理退出: %v\n", err)
			}
		}()
		fmt.Printf("[ai-trace] MITM 代理监听 http://%s（HTTPS_PROXY 用；需信任 CA：%s）\n",
			mitmAddr, filepath.Join(dataDir(), "ca", "cert.pem"))
	} else {
		fmt.Fprintf(os.Stderr, "[ai-trace] CA 初始化失败，MITM 未启动: %v\n", caErr)
	}

	fmt.Printf("[ai-trace] 网关+Dashboard 监听 http://%s（数据 %s）\n", addr, st.Root())
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "监听失败: %v\n", err)
		return 1
	}
	return 0
}

// runCmd ai-trace run。
func runCmd(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	if args[0] == "--" {
		return run.Run("--", args[1:])
	}
	return run.Run(args[0], args[1:])
}

// openCmd 打开 Dashboard。
func openCmd() int {
	run.EnsureServing()
	url := "http://127.0.0.1:8787"
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
	fmt.Println("Dashboard:", url)
	return 0
}

// mitmAddr / gatewayAddr 常量集中管理。
const (
	gatewayAddr = "127.0.0.1:8787"
	mitmAddr    = "127.0.0.1:8788"
)

// captureCmd 系统代理开关（MITM 模式）。
// 逻辑在 internal/capture（与 Dashboard /api/capture、桌面客户端共用）。
func captureCmd(args []string) int {
	if len(args) == 0 || (args[0] != "on" && args[0] != "off") {
		fmt.Println("用法: ai-trace capture on|off")
		return 2
	}
	if args[0] == "off" {
		prev, err := capture.Off(dataDir())
		if err != nil {
			fmt.Fprintf(os.Stderr, "还原系统代理失败: %v（请到系统设置手动改回 %q）\n", err, prev)
			return 1
		}
		if prev != "" {
			fmt.Printf("已还原系统代理 → %s\n", prev)
		} else {
			fmt.Println("已关闭系统代理")
		}
		return 0
	}
	// on：开系统代理 + CA 信任引导
	certPath, err := capture.On(dataDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "开启失败: %v\n", err)
		return 1
	}
	fmt.Println(`系统代理将指向 ai-trace 的 MITM 代理（127.0.0.1:8788）。
抓 HTTPS 流量需要客户端信任 ai-trace CA。推荐环境变量方式（不动系统信任链）：
  export NODE_EXTRA_CA_CERTS='` + certPath + `'   # Node 系 agent
  export SSL_CERT_FILE='` + certPath + `'         # Go / Python(requests) 系
  export REQUESTS_CA_BUNDLE='` + certPath + `'
桌面应用（Electron 等）信任系统证书链，安装一次：
  macOS:   sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ` + certPath + `
  Linux:   sudo cp ` + certPath + ` /usr/local/share/ca-certificates/ai-trace.crt && sudo update-ca-certificates
  Windows: certutil -addstore Root ` + certPath)
	fmt.Println()
	fmt.Println("用完记得: ai-trace capture off")
	return 0
}

// exportCmd 导出账本与统计为 JSON。
//
//	ai-trace export                 # 最近 7 天
//	ai-trace export --day 2026-10-17
//	ai-trace export --out report.json
func exportCmd(args []string) int {
	day, out := "", "ai-trace-report.json"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--day":
			if i+1 < len(args) {
				day = args[i+1]
				i++
			}
		case "--out":
			if i+1 < len(args) {
				out = args[i+1]
				i++
			}
		}
	}
	st, err := store.Open(dataDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据目录失败: %v\n", err)
		return 1
	}
	days := st.Days()
	if day != "" {
		days = []string{day}
	} else if len(days) > 7 {
		days = days[:7]
	}
	type dayData struct {
		Day     string          `json:"day"`
		Records []store.Record  `json:"records"`
	}
	var all []dayData
	for _, d := range days {
		recs, err := st.Records(d)
		if err != nil {
			continue
		}
		all = append(all, dayData{Day: d, Records: recs})
	}
	report := map[string]any{
		"generatedAt": time.Now().Format(time.RFC3339),
		"stats":       analytics.Analyze(st, analytics.LoadPriceTable(dataDir()), 30),
		"days":        all,
	}
	data, err := json.MarshalIndent(report, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化失败: %v\n", err)
		return 1
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
		return 1
	}
	fmt.Printf("已导出 %d 天记录 → %s（%.1f KB）\n", len(all), out, float64(len(data))/1024)
	return 0
}

// doctorCmd 诊断：agent 检测 + 接入建议 + 服务状态。
func doctorCmd() int {
	fmt.Println("== ai-trace 诊断 ==")
	// 服务状态
	fmt.Println("[服务]")
	gwOK := probeHTTP("http://" + gatewayAddr + "/healthz")
	mitmOK := probeTCP(mitmAddr)
	fmt.Printf("  网关 %s %s\n", gatewayAddr, statusMark(gwOK))
	fmt.Printf("  MITM %s %s\n", mitmAddr, statusMark(mitmOK))
	if !gwOK {
		fmt.Println("  → 未运行：任意 ai-trace run/open 会自动拉起，或手动 ai-trace serve")
	}
	// CA
	caDir := filepath.Join(dataDir(), "ca")
	_, caErr := os.Stat(filepath.Join(caDir, "cert.pem"))
	fmt.Printf("\n[CA] %s\n", filepath.Join(caDir, "cert.pem"))
	if caErr == nil {
		fmt.Println("  已生成（capture on 时会给出各平台信任引导）")
	} else {
		fmt.Println("  未生成（首次 capture on 或 serve 时自动生成）")
	}
	// agent
	fmt.Println("\n[本机 agent 与建议接入方式]")
	inst := adapter.Installed()
	if len(inst) == 0 {
		fmt.Println("  未检测到已安装的 agent")
	}
	for _, a := range inst {
		fmt.Printf("  %-16s %s\n", a.ID, a.Name)
		fmt.Printf("  %s\n", runAdvice(a))
	}
	fmt.Println("\n[其他]")
	fmt.Println("  未装 base_url 的程序 → ai-trace capture on（MITM 模式，需信任 CA）")
	fmt.Println("  任意命令试一把       → ai-trace run -- <cmd>")
	return 0
}

// runAdvice 一个 agent 的接入建议文案。
func runAdvice(a *adapter.Adapter) string {
	switch a.Kind {
	case "desktop":
		return "桌面端：优先在设置里找自定义 API 地址；没有则 capture on 走 MITM"
	default:
		return fmt.Sprintf("运行: ai-trace run %s  （协议 %s，env 注入零配置）", a.ID, a.Protocol)
	}
}

func statusMark(ok bool) string {
	if ok {
		return "✓ 运行中"
	}
	return "✗ 未运行"
}

// probeHTTP 探活 HTTP 服务。
func probeHTTP(url string) bool {
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// probeTCP 探活 TCP 端口。
func probeTCP(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// adaptersCmd 列出 agent。
func adaptersCmd() int {
	fmt.Println("本机已安装（可用 ai-trace run 追踪启动）:")
	for _, a := range adapter.Installed() {
		fmt.Printf("  %-18s %-22s 协议 %s\n", a.ID, a.Name, a.Protocol)
	}
	inst := map[string]bool{}
	for _, a := range adapter.Installed() {
		inst[a.ID] = true
	}
	fmt.Println("\n内置适配器（未检测到安装）:")
	for _, a := range adapter.Registry() {
		if !inst[a.ID] {
			fmt.Printf("  %-18s %-22s 协议 %s\n", a.ID, a.Name, a.Protocol)
		}
	}
	fmt.Println("\n任意支持自定义 base_url 的程序: ai-trace run -- <命令>")
	return 0
}

// skillsBackfillCmd 从全部历史录制流量回填技能沉淀（~/.ai-trace/skills/）。
// 使用事件索引先清空再重建（正文按指纹去重，重复回填无副作用）。
func skillsBackfillCmd() int {
	root := dataDir()
	st, err := store.Open(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据目录失败: %v\n", err)
		return 1
	}
	os.Remove(filepath.Join(root, "skills", "index.jsonl"))
	nSkill := 0
	for _, day := range st.Days() {
		recs, err := st.Records(day)
		if err != nil {
			continue
		}
		for _, rec := range recs {
			b, err := st.Body(rec.ID)
			if err != nil || len(b.RequestBody) == 0 {
				continue
			}
			nSkill += skills.Capture(root, b.RequestBody, skills.Meta{
				Agent: rec.Agent, Session: rec.Session, Model: rec.Model, At: rec.Time,
			})
		}
	}
	list := skills.List(root)
	fmt.Printf("回填完成：扫描 %d 天，命中 %d 条技能注入，沉淀 %d 个技能\n",
		len(st.Days()), nSkill, len(list))
	for _, it := range list {
		fmt.Printf("  %-40s %3d 次使用  %6d B  %s\n", it.Name, it.Usages, it.Size, it.LastSeen)
	}
	return 0
}
