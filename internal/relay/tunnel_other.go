//go:build !windows

package relay

import "os/exec"

func hideSSHWindow(cmd *exec.Cmd) {}
