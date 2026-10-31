//go:build windows

package run

import (
	"os/exec"
	"syscall"
)

// Windows 进程创建标志（syscall 包未导出 DETACHED_PROCESS，自行定义）
const (
	detachedProcess    = 0x00000008
	createNewProcGroup = 0x00000200
)

// setSysProcAttr Windows 上创建新进程组以便信号传递。
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcGroup}
}

// detach 后台网关进程脱离当前控制台（DETACHED）。
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcGroup}
}
