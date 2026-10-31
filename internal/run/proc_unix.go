//go:build !windows

package run

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr 子进程保持默认进程组（与 run 同组，Ctrl+C 一起收到）。
func setSysProcAttr(cmd *exec.Cmd) {}

// detach 让后台网关进程脱离当前进程组（新会话），Ctrl+C 不会杀掉它。
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
