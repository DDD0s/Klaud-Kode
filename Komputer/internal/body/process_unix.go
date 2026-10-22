//go:build !windows

package body

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func setCmdLine(cmd *exec.Cmd, line string) {}

func exitInfo(state *os.ProcessState) (*int, *string) {
	if state == nil {
		return nil, nil
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		sig := status.Signal().String()
		return nil, &sig
	}
	code := state.ExitCode()
	return &code, nil
}

func fileMode(info os.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Mode & 0o7777)
	}
	return int(info.Mode().Perm())
}

func birthTime(info os.FileInfo) time.Time { return info.ModTime() }
