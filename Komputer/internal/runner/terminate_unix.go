//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// terminateProcess asks the CLI and its process group to stop, then kills the
// group if it is still alive after a short grace period.
func terminateProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.AfterFunc(5*time.Second, func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	})
}
