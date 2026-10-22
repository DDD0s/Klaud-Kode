package body

import (
	"os/exec"
	"regexp"
	"strings"
)

func osVersion() string {
	out, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "(Get-CimInstance Win32_OperatingSystem).Caption").Output()
	if err == nil {
		if text := strings.TrimSpace(string(out)); text != "" {
			return text
		}
	}
	out, err = exec.Command("cmd.exe", "/d", "/c", "ver").Output()
	if err != nil {
		return "Windows"
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "Windows"
	}
	return text
}

var windowsReleasePattern = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+`)

func osRelease() string {
	out, err := exec.Command("cmd.exe", "/d", "/c", "ver").Output()
	if err != nil {
		return ""
	}
	return windowsReleasePattern.FindString(string(out))
}
