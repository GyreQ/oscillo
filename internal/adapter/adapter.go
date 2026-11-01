// Package adapter 声明式 agent 适配器：描述一个 agent 怎么被识别和接入。
// 与 magpie 的手写 Go 适配器不同，我们不改写 agent 配置（env 注入，退出即还原），
// 所以适配器是纯数据（JSON），新增 agent 只需加一个文件。
package adapter

import (
	"embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed adapters/*.json
var adapterFS embed.FS

// Adapter 一个 agent 的接入描述。
type Adapter struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`        // cli | desktop | web
	Protocol    string            `json:"protocol"`    // anthropic | openai-chat | openai-responses | gemini
	Bins        []string          `json:"bins"`        // 可执行文件名（检测用）
	Apps        []string          `json:"apps"`        // 桌面应用名（macOS /Applications 下检测）
	ConfigFiles []string          `json:"configFiles"` // 配置文件路径（~/ 展开，存在即视为已安装）
	Env         map[string]string `json:"env"`         // run 时注入的环境变量
	// Launch 自定义启动命令（优先于 Bins）：[0] 可执行文件，其余为固定参数，
	// run 的附加参数接在后面。条目里的 {{app}} 展开为命中的 .app 包路径
	// （如 Electron 壳以 node 模式跑内置 CLI 的场景）。
	Launch     []string          `json:"launch,omitempty"`
	Note       string            `json:"note,omitempty"` // 接入说明（dashboard 显示）
	Advice     string            `json:"advice,omitempty"` // doctor 的接入建议（缺省按 kind 生成）
	UserAgents []string          `json:"userAgents"`       // 归因用的 UA 前缀（信息性）
	Logs       []string          `json:"logs,omitempty"`   // 本地会话日志路径（Phase 2 用）
}

// Registry 加载全部内置适配器。
func Registry() []*Adapter {
	ents, err := adapterFS.ReadDir("adapters")
	if err != nil {
		return nil
	}
	var out []*Adapter
	for _, e := range ents {
		data, err := adapterFS.ReadFile("adapters/" + e.Name())
		if err != nil {
			continue
		}
		var a Adapter
		if json.Unmarshal(data, &a) == nil && a.ID != "" {
			out = append(out, &a)
		}
	}
	return out
}

// ByID 按 id 查找。
func ByID(id string) *Adapter {
	for _, a := range Registry() {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// Detect 检测一个适配器是否已安装：可执行文件、桌面应用、配置文件任一命中。
func (a *Adapter) Detect() bool {
	for _, bin := range a.Bins {
		if bin == "" {
			continue
		}
		// Windows 可执行文件带 .exe 后缀
		if runtime.GOOS == "windows" {
			if strings.HasSuffix(bin, ".exe") {
				if _, err := exec.LookPath(bin); err == nil {
					return true
				}
			} else if _, err := exec.LookPath(bin + ".exe"); err == nil {
				return true
			}
			continue
		}
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	// macOS 桌面应用（也看用户级 ~/Applications）
	if runtime.GOOS == "darwin" {
		for _, app := range a.Apps {
			for _, dir := range []string{"/Applications", filepath.Join(homeDir(), "Applications")} {
				if _, err := os.Stat(filepath.Join(dir, app)); err == nil {
					return true
				}
			}
		}
	}
	// 配置目录/文件存在（Linux 桌面应用与跨平台兜底）
	for _, cf := range a.ConfigFiles {
		if cf == "" {
			continue
		}
		if _, err := os.Stat(ExpandHome(cf)); err == nil {
			return true
		}
	}
	return false
}

// AppPath 解析首个存在的桌面应用包路径（macOS：/Applications 与 ~/Applications）。
// 供 Launch 的 {{app}} 占位符用；找不到或非 macOS 返回空串。
func (a *Adapter) AppPath() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	for _, app := range a.Apps {
		for _, dir := range []string{"/Applications", filepath.Join(homeDir(), "Applications")} {
			p := filepath.Join(dir, app)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// ExpandLaunch 展开启动命令里的 {{app}} 占位符。应用包不存在时 ok=false
//（无法定位可执行文件）。
func (a *Adapter) ExpandLaunch() ([]string, bool) {
	if len(a.Launch) == 0 {
		return nil, false
	}
	app := a.AppPath()
	out := make([]string, 0, len(a.Launch))
	for _, p := range a.Launch {
		if strings.Contains(p, "{{app}}") {
			if app == "" {
				return nil, false
			}
			p = strings.ReplaceAll(p, "{{app}}", app)
		}
		out = append(out, p)
	}
	return out, true
}

// homeDir 当前用户主目录（失败返回空串）。
func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// Installed 返回本机已安装的适配器。
func Installed() []*Adapter {
	var out []*Adapter
	for _, a := range Registry() {
		if a.Detect() {
			out = append(out, a)
		}
	}
	return out
}

// ExpandHome 展开 ~ 与环境变量（日志路径用）。
func ExpandHome(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
