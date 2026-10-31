// Package capture 系统代理接管（MITM 模式）的开 / 关 / 状态。
// CLI（ai-trace capture）、Dashboard API（/api/capture）、桌面客户端托盘
// 三方共用这一份逻辑。
//
// on 会先备份当前代理到 <dataDir>/proxy-backup.json，off 还原它——
// 不会弄丢用户原有的代理（如本机 Clash）。
package capture

import (
	"encoding/json"
	"os"
	"path/filepath"

	"ai-trace/internal/mitm"
	"ai-trace/internal/sysproxy"
)

// MITMAddr MITM 代理监听地址（与 serveCmd 保持一致）。
const MITMAddr = "127.0.0.1:8788"

// State 当前状态。
type State struct {
	On    bool   `json:"on"`    // 系统代理是否已指向 MITM
	Proxy string `json:"proxy"` // 当前系统代理（host:port，空 = 未设）
}

// On 开启：备份当前代理 → 确保 CA 存在 → 系统代理指向 MITM。
// 返回 CA 证书路径（供信任引导）与提示信息。
func On(dataDir string) (certPath string, err error) {
	backupPath := filepath.Join(dataDir, "proxy-backup.json")
	// 备份当前代理（若已是 8788 不重复备份）
	cur, rerr := sysproxy.Read()
	if rerr == nil && cur != "" && cur != MITMAddr {
		if b, jerr := json.Marshal(map[string]string{"proxy": cur}); jerr == nil {
			_ = os.WriteFile(backupPath, b, 0o644)
		}
	}
	// 确保 CA 存在（首次生成）
	caDir := filepath.Join(dataDir, "ca")
	if _, err = mitm.LoadCA(caDir); err != nil {
		return "", err
	}
	certPath = filepath.Join(caDir, "cert.pem")
	if err = sysproxy.On(MITMAddr); err != nil {
		return certPath, err
	}
	return certPath, nil
}

// Off 关闭：有备份则还原原代理（可能是用户的 Clash），没有则直接关闭。
// 返回还原后的代理地址（空 = 已全部关闭）。
func Off(dataDir string) (restored string, err error) {
	backupPath := filepath.Join(dataDir, "proxy-backup.json")
	prev := ""
	if b, rerr := os.ReadFile(backupPath); rerr == nil {
		var m struct {
			Proxy string `json:"proxy"`
		}
		if json.Unmarshal(b, &m) == nil {
			prev = m.Proxy
		}
	}
	if err = sysproxy.Restore(prev); err != nil {
		return prev, err
	}
	os.Remove(backupPath)
	return prev, nil
}

// Read 状态：读当前系统代理，判断是否指向 MITM。
func Read() State {
	cur, err := sysproxy.Read()
	if err != nil {
		return State{}
	}
	return State{On: cur == MITMAddr, Proxy: cur}
}
