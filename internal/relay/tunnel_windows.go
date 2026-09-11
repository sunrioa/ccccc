//go:build windows

package relay

import (
	"os/exec"
	"syscall"
)

func hideSSHWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }
