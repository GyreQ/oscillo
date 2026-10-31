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
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`         // cli | desktop | web
	Protocol     string            `json:"protocol"`     // anthropic | openai-chat | openai-responses | gemini
	Bins         []string          `json:"bins"`         // 可执行文件名（检测用）
	Apps         []string          `json:"apps"`         // 桌面应用名（macOS /Applications 下检测）
	ConfigFiles  []string          `json:"configFiles"`  // 配置文件路径（~/ 展开，存在即视为已安装）
	Env          map[string]string `json:"env"`          // run 时注入的环境变量
	Note         string            `json:"note,omitempty"` // 接入说明（dashboard 显示）
	UserAgents   []string          `json:"userAgents"`   // 归因用的 UA 前缀（信息性）
	Logs         []string          `json:"logs,omitempty"` // 本地会话日志路径（Phase 2 用）
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
