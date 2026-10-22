package body

import (
	"os/exec"
	"strings"
)

func osVersion() string {
	out, err := exec.Command("cmd.exe", "/d", "/c", "ver").Output()
	if err != nil {
		return "Windows"
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "Windows"
	}
	return text
}
