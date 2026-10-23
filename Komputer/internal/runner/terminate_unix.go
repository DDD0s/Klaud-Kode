//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

func terminateProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	time.AfterFunc(5*time.Second, func() {
		_ = cmd.Process.Kill()
	})
}
