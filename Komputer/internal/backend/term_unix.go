//go:build !windows

package backend

import (
	"os/exec"
	"syscall"
)

func termProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
}
