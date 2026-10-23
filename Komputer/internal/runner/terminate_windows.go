package runner

import (
	"os/exec"
	"strconv"
)

// terminateProcess ends the CLI and anything it spawned. The CLI launches
// child processes, so a bare Kill would orphan them on Windows.
func terminateProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/pid", strconv.Itoa(cmd.Process.Pid), "/t", "/f").Run()
}
