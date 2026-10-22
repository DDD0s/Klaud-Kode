package body

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/pid", strconv.Itoa(cmd.Process.Pid), "/t", "/f").Run()
}

func setCmdLine(cmd *exec.Cmd, line string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
}

func exitInfo(state *os.ProcessState) (*int, *string) {
	if state == nil {
		return nil, nil
	}
	if state.Exited() {
		code := state.ExitCode()
		return &code, nil
	}
	return nil, nil
}

func fileMode(info os.FileInfo) int { return int(info.Mode().Perm()) }

func birthTime(info os.FileInfo) time.Time {
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, data.CreationTime.Nanoseconds()).UTC()
	}
	return info.ModTime()
}
