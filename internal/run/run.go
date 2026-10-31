// Package run 实现 `ai-trace run`：注入 base_url 环境变量后启动目标 agent，
// 网关不在跑时自动后台拉起。退出码透传，用户 Ctrl+C 正常传给子进程。
package run

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ai-trace/internal/adapter"
)

// DefaultAddr 网关默认地址。
const DefaultAddr = "127.0.0.1:8787"

// Run 执行 run 命令。
//
//	ai-trace run claude <args...>   # 按适配器注入，启动该 agent
//	ai-trace run -- <cmd> <args...> # 通用模式：注入全部已知 base_url 变量
//
// 返回子进程退出码。
func Run(adapterID string, args []string) int {
	if len(args) == 0 {
		fmt.Println("用法: ai-trace run <adapter> [args...]   # 如 ai-trace run claude")
		fmt.Println("      ai-trace run -- <cmd> [args...]    # 如 ai-trace run -- python my_agent.py")
		return 2
	}

	ensureServing()

	// 组装命令与环境变量
	var cmdName string
	var cmdArgs []string
	envMap := map[string]string{}
	if adapterID == "--" || adapterID == "" {
		// 通用模式：命令由用户给出，注入全部已知 base_url（读到哪个算哪个）
		cmdName, cmdArgs = args[0], args[1:]
		for _, a := range adapter.Registry() {
			for k, v := range a.Env {
				if isBaseURLVar(k) && envMap[k] == "" {
					envMap[k] = v
				}
			}
		}
	} else {
		a := adapter.ByID(adapterID)
		if a == nil {
			fmt.Printf("未知 agent: %s（ai-trace adapters 查看内置列表）\n", adapterID)
			return 2
		}
		if len(a.Bins) == 0 {
			fmt.Printf("%s 是通用模板，请用: ai-trace run -- <你的命令>\n", a.Name)
			return 2
		}
		cmdName, cmdArgs = a.Bins[0], args
		for k, v := range a.Env {
			envMap[k] = v
		}
	}

	env := os.Environ()
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}

	fmt.Fprintf(os.Stderr, "[ai-trace] 启动 %s（流量录制中 → http://%s）\n", cmdName, DefaultAddr)
	for k, v := range envMap {
		fmt.Fprintf(os.Stderr, "[ai-trace]   %s=%s\n", k, v)
	}

	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	setSysProcAttr(cmd) // 平台差异：子进程组设置

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[ai-trace] 启动失败: %v\n", err)
		return 127
	}

	// 捕获终止信号转发给子进程（Ctrl+C 时子进程通常也会经进程组收到，
	// 这里兜底转发并等待其自行清理退出，退出码透传）
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigs {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(s)
			}
		}
	}()

	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "[ai-trace] 等待子进程失败: %v\n", err)
	return 1
}

// isBaseURLVar 判断是否为 base_url 类变量（通用模式只注入这类）。
func isBaseURLVar(k string) bool {
	return len(k) > 9 && k[len(k)-9:] == "_BASE_URL"
}

// EnsureServing 导出给 open 命令复用：网关不在跑则后台拉起。
func EnsureServing() { ensureServing() }

// alive 探活网关。
func alive() bool {
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + DefaultAddr + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return true
}

// ensureServing 网关不在跑则后台拉起（脱离进程组，Ctrl+C 不杀网关）。
func ensureServing() {
	if alive() {
		return
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ai-trace] 找不到自身可执行文件: %v\n", err)
		return
	}
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, ".ai-trace", "serve.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logF = os.Stderr
	} else {
		defer logF.Close()
	}
	cmd := exec.Command(self, "serve")
	cmd.Stdout = logF
	cmd.Stderr = logF
	detach(cmd) // 平台差异：脱离控制进程组
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[ai-trace] 后台网关启动失败: %v（日志 %s）\n", err, logPath)
		return
	}
	// 轮询等就绪
	for i := 0; i < 50; i++ {
		if alive() {
			fmt.Fprintf(os.Stderr, "[ai-trace] 网关已就绪（日志 %s）\n", logPath)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "[ai-trace] 网关启动超时，详见 %s；请求可能不会被录制\n", logPath)
}
