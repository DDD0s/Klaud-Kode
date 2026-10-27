package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The stage is the set the brain lives on: a directory tree holding every
// environment file the CLI can read about itself, filled with the caller's
// real values. A launcher later mounts this tree over the guest's own files
// (see launch.sh), so every probe the CLI makes answers with this computer.

// StageOptions controls what BuildStage writes.
type StageOptions struct {
	// Rootfs is the stage directory to create (must not be "/").
	Rootfs string
	// DryRun reports the planned files without writing.
	DryRun bool
}

// StageFile is one planned file in the stage tree.
type StageFile struct {
	Path    string
	Content string
	Mode    os.FileMode
}

// Plan returns the files BuildStage would write, without touching disk.
// Tests and dry runs use it to prove the stage is internally consistent.
func Plan(p Profile) []StageFile {
	var files []StageFile
	add := func(path, content string, mode os.FileMode) {
		files = append(files, StageFile{Path: path, Content: content, Mode: mode})
	}
	add("etc/hostname", p.Hostname+"\n", 0o644)
	add("etc/machine-id", p.MachineID+"\n", 0o444)
	add("var/lib/dbus/machine-id", p.MachineID+"\n", 0o644)
	add("etc/hosts", hostsContent(p.Hostname), 0o644)
	if p.Locale != "" {
		add("etc/locale.conf", "LANG="+p.Locale+"\n", 0o644)
		add("etc/default/locale", "LANG="+p.Locale+"\n", 0o644)
	}
	if osr := osReleaseContent(p); osr != "" {
		add("etc/os-release", osr, 0o644)
	}
	if cpu := cpuinfoContent(p); cpu != "" {
		add("proc/cpuinfo", cpu, 0o444)
	}
	if mem := meminfoContent(p); mem != "" {
		add("proc/meminfo", mem, 0o444)
	}
	// DMI: single-line firmware facts the kernel exposes under /sys/class/dmi.
	dmi := map[string]string{
		"sys_vendor": p.SMBIOS.Manufacturer,
		// Never fill firmware facts with the hostname: an absent field is
		// honest, an invented one is a tell.
		"product_name":    p.SMBIOS.Product,
		"product_version": p.SMBIOS.Version,
		"product_serial":  p.SMBIOS.Serial,
		"product_uuid":    p.SystemUUID,
		"product_family":  p.SMBIOS.Family,
		"product_sku":     p.SMBIOS.SKU,
		"board_vendor":    p.SMBIOS.BoardManufacturer,
		"board_name":      p.SMBIOS.BoardProduct,
		"board_serial":    p.SMBIOS.BoardSerial,
	}
	for name, value := range dmi {
		if strings.TrimSpace(value) != "" {
			add("sys/class/dmi/id/"+name, strings.TrimSpace(value)+"\n", 0o444)
		}
	}
	// Timezone by name; the launcher links /etc/localtime to this file in
	// the guest's own zoneinfo so the staged copy is informational.
	if p.Timezone.IANA != "" {
		add("etc/timezone", p.Timezone.IANA+"\n", 0o644)
	}
	// The environment the CLI process is started with.
	add("stage.env", stageEnv(p), 0o600)
	return files
}

// BuildStage writes Plan(p) under opts.Rootfs and returns what it did.
func BuildStage(p Profile, opts StageOptions) (ApplyReport, error) {
	var report ApplyReport
	if opts.Rootfs == "" || opts.Rootfs == "/" {
		return report, fmt.Errorf("stage root must be a dedicated directory, got %q", opts.Rootfs)
	}
	if err := p.Validate(); err != nil {
		return report, err
	}
	// The staged home sits beside the identity files, named after the user so
	// paths inside it read like the real home directory.
	homeRel := "home"
	if p.User.Name != "" {
		homeRel = filepath.Join("home", p.User.Name)
	}
	if opts.DryRun {
		for _, f := range Plan(p) {
			report.note("would write %s", filepath.Join(opts.Rootfs, f.Path))
		}
		report.note("would create %s", filepath.Join(opts.Rootfs, homeRel))
		return report, nil
	}
	for _, f := range Plan(p) {
		dest := filepath.Join(opts.Rootfs, f.Path)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return report, fmt.Errorf("stage %s: %w", f.Path, err)
		}
		if err := os.WriteFile(dest, []byte(f.Content), f.Mode); err != nil {
			return report, fmt.Errorf("stage %s: %w", f.Path, err)
		}
		report.note("staged %s", f.Path)
	}
	if err := os.MkdirAll(filepath.Join(opts.Rootfs, homeRel), 0o755); err != nil {
		return report, fmt.Errorf("stage home: %w", err)
	}
	report.note("created %s", homeRel)
	return report, nil
}

func hostsContent(hostname string) string {
	return "127.0.0.1 localhost\n127.0.1.1 " + hostname + "\n::1 localhost ip6-localhost ip6-loopback\n"
}

// osReleaseContent describes the system the guest kernel cannot fake. On a
// Linux VPS the kernel stays Linux; this file only carries the caller's naming
// so tools that read it do not leak the server distribution.
func osReleaseContent(p Profile) string {
	if p.OS.Name == "" {
		return ""
	}
	pretty := p.OS.Name
	if p.OS.Version != "" && !strings.Contains(pretty, p.OS.Version) {
		pretty += " " + p.OS.Version
	}
	var b strings.Builder
	b.WriteString("NAME=\"" + p.OS.Name + "\"\n")
	b.WriteString("PRETTY_NAME=\"" + pretty + "\"\n")
	if p.OS.Version != "" {
		b.WriteString("VERSION_ID=\"" + p.OS.Version + "\"\n")
	}
	b.WriteString("ID=kloud\n")
	return b.String()
}

func cpuinfoContent(p Profile) string {
	if p.CPU.Model == "" {
		return ""
	}
	var b strings.Builder
	threads := p.CPU.Threads
	if threads < 1 {
		threads = 1
	}
	for i := 0; i < threads; i++ {
		fmt.Fprintf(&b, "processor\t: %d\n", i)
		if p.CPU.Vendor != "" {
			fmt.Fprintf(&b, "vendor_id\t: %s\n", p.CPU.Vendor)
		}
		fmt.Fprintf(&b, "model name\t: %s\n", p.CPU.Model)
		if p.CPU.Cores > 0 {
			fmt.Fprintf(&b, "cpu cores\t: %d\n", p.CPU.Cores)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func meminfoContent(p Profile) string {
	if p.MemoryMB <= 0 {
		return ""
	}
	kb := strconv.Itoa(p.MemoryMB * 1024)
	return "MemTotal:       " + kb + " kB\n" +
		"MemFree:        " + kb + " kB\n" +
		"MemAvailable:   " + kb + " kB\n" +
		"SwapTotal:             0 kB\n" +
		"SwapFree:              0 kB\n"
}

// stageEnv renders the environment the staged process starts with. These are
// the values a CLI reads before it touches the filesystem at all.
func stageEnv(p Profile) string {
	var b strings.Builder
	put := func(key, value string) {
		if value != "" {
			b.WriteString(key + "=" + value + "\n")
		}
	}
	put("HOSTNAME", p.Hostname)
	put("USER", p.User.Name)
	put("LOGNAME", p.User.Name)
	if p.User.Name != "" {
		put("HOME", "/home/"+p.User.Name)
	}
	put("LANG", p.Locale)
	put("LC_ALL", p.Locale)
	put("TZ", p.Timezone.IANA)
	put("KL_IDENTITY_NIC_MAC", func() string {
		if nic, ok := p.PrimaryNIC(); ok {
			return nic.MAC
		}
		return ""
	}())
	return b.String()
}
