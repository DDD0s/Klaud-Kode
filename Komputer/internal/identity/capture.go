package identity

import (
	"fmt"
	"net"
	"os"
	osuser "os/user"
	"runtime"
	"strings"
	"time"
)

// Capture reads the identity of the machine it runs on. Run it on the
// caller's own computer; run Apply on the brain.
func Capture() (Profile, error) {
	p := Profile{
		Schema:     Schema,
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		SourceOS:   runtime.GOOS,
		OS:         OS{Arch: runtime.GOARCH},
	}

	if h, err := os.Hostname(); err == nil {
		p.Hostname = h
	}
	u, err := osuser.Current()
	if err == nil {
		p.User.Name = shortUserName(u.Username)
		p.User.Home = u.HomeDir
	}
	if p.User.Name == "" {
		if n := os.Getenv("USERNAME"); n != "" {
			p.User.Name = n
		} else {
			p.User.Name = os.Getenv("USER")
		}
	}
	if p.User.Home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			p.User.Home = h
		}
	}
	p.User.Shell = firstNonEmpty(os.Getenv("ComSpec"), os.Getenv("SHELL"))
	p.CPU.Threads = runtime.NumCPU()
	p.NICs = listNICs()

	if err := capturePlatform(&p); err != nil {
		return p, fmt.Errorf("capture platform facts: %w", err)
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// localeFromCulture turns a BCP-47 name like zh-CN into the locale string a
// Linux guest expects.
func localeFromCulture(culture string) string {
	culture = strings.TrimSpace(culture)
	if culture == "" {
		return ""
	}
	return strings.ReplaceAll(culture, "-", "_") + ".UTF-8"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// shortUserName drops a Windows DOMAIN prefix.
func shortUserName(name string) string {
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func listNICs() []NIC {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []NIC
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) == 0 {
			continue
		}
		mac := iface.HardwareAddr.String()
		if mac == "00:00:00:00:00:00" {
			continue
		}
		out = append(out, NIC{MAC: mac, Description: iface.Name})
	}
	return out
}
