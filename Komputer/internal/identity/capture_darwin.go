//go:build darwin

package identity

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func sysctl(name string) string {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func capturePlatform(p *Profile) error {
	// macOS has no machine-id; the hardware UUID is the stable identity.
	p.SystemUUID = sysctl("kern.uuid")
	p.MachineID = MachineIDFromGUID(p.SystemUUID)
	p.SMBIOS.Manufacturer = "Apple Inc."
	p.SMBIOS.Product = sysctl("hw.model")
	p.CPU.Model = sysctl("machdep.cpu.brand_string")
	if n := sysctl("hw.physicalcpu"); n != "" {
		if v, err := strconv.Atoi(n); err == nil {
			p.CPU.Cores = v
		}
	}
	if mem := sysctl("hw.memsize"); mem != "" {
		if v, err := strconv.ParseInt(mem, 10, 64); err == nil {
			p.MemoryMB = int(v / 1024 / 1024)
		}
	}
	p.OS.Name = "macOS"
	p.OS.Version = sysctl("kern.osproductversion")
	p.OS.Build = sysctl("kern.osversion")
	zone, _ := time.Now().Zone()
	_ = zone
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.LastIndex(link, "zoneinfo/"); i >= 0 {
			p.Timezone.IANA = link[i+len("zoneinfo/"):]
		}
	}
	if lang := os.Getenv("LANG"); lang != "" {
		p.Locale = lang
	}
	return nil
}
