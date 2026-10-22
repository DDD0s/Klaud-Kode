//go:build !windows

package body

import (
	"os"
	"os/exec"
	"strings"
)

func osVersion() string {
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), string([]byte{10})) {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), string([]byte{34}))
			}
		}
	}
	out, err := exec.Command("uname", "-srm").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return "unix"
}
