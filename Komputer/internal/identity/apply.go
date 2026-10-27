// Package identity (apply side): overwrites the identity facts a Linux guest
// reports with the caller's real ones, so the guest stops looking like the
// Linux server it happens to run on.
package identity

import (
	"fmt"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ApplyOptions controls how aggressive an application is.
type ApplyOptions struct {
	// DryRun reports what would change without writing anything.
	DryRun bool
	// CreateUser creates the caller's account if it does not exist yet.
	CreateUser bool
	// RootPrefix writes under this directory instead of "/". Used to stage a
	// guest root filesystem offline (and by tests); live system commands such
	// as hostnamectl/useradd are skipped in this mode.
	RootPrefix string
}

// ApplyReport records each step that ran or was skipped, for auditability.
type ApplyReport struct {
	Steps []string
}

func (r *ApplyReport) note(format string, args ...any) {
	r.Steps = append(r.Steps, fmt.Sprintf(format, args...))
}

func (r *ApplyReport) String() string {
	return strings.Join(r.Steps, "\n")
}

// Apply makes the current Linux system report p's identity. It must run as
// root. Facts that live in firmware or on the NIC/disk are not touched here;
// those come from the QEMU arguments generated from the same profile.
func Apply(p Profile, opts ApplyOptions) (ApplyReport, error) {
	var report ApplyReport
	offline := opts.RootPrefix != ""
	if !offline && runtime.GOOS != "linux" {
		return report, fmt.Errorf("apply is only supported on linux guests (or use --root to stage a rootfs)")
	}
	at := func(abs string) string {
		if !offline {
			return abs
		}
		return filepath.Join(opts.RootPrefix, strings.TrimLeft(abs, "/"))
	}
	if err := p.Validate(); err != nil {
		return report, err
	}

	// Hostname.
	if err := writeIfChanged(&report, at("/etc/hostname"), p.Hostname+"\n", 0o644, opts.DryRun); err != nil {
		return report, err
	}
	if offline {
		report.note("hostname applies on first guest boot (offline rootfs)")
	} else if err := runOptional(&report, "hostnamectl", "set-hostname", p.Hostname); err != nil && !opts.DryRun {
		report.note("hostnamectl skipped: %v", err)
	}

	// Machine id: dbus follows /etc/machine-id on most distros.
	if err := writeIfChanged(&report, at("/etc/machine-id"), p.MachineID+"\n", 0o444, opts.DryRun); err != nil {
		return report, err
	}
	if info, err := os.Lstat(at("/var/lib/dbus/machine-id")); err == nil && info.Mode()&os.ModeSymlink == 0 {
		if err := writeIfChanged(&report, at("/var/lib/dbus/machine-id"), p.MachineID+"\n", 0o644, opts.DryRun); err != nil {
			return report, err
		}
	}

	// Timezone.
	zone := p.Timezone.IANA
	if zone == "" {
		zone = etcZoneFromOffset(p.Timezone.UTCOffset)
	}
	if zone != "" {
		target := filepath.Join(at("/usr/share/zoneinfo"), zone)
		if _, err := os.Stat(target); err == nil {
			if opts.DryRun {
				report.note("would set /etc/localtime -> %s", zone)
			} else {
				_ = os.Remove(at("/etc/localtime"))
				if err := os.Symlink(target, at("/etc/localtime")); err == nil {
					report.note("set /etc/localtime -> %s", zone)
				} else {
					// A copied zone file works where symlink privilege is absent
					// (staging a rootfs from Windows without Developer Mode).
					data, readErr := os.ReadFile(target)
					if readErr != nil {
						return report, fmt.Errorf("set timezone: %w", err)
					}
					if err := os.MkdirAll(filepath.Dir(at("/etc/localtime")), 0o755); err != nil {
						return report, fmt.Errorf("create directory for /etc/localtime: %w", err)
					}
					if err := os.WriteFile(at("/etc/localtime"), data, 0o644); err != nil {
						return report, fmt.Errorf("set timezone: %w", err)
					}
					report.note("copied %s to /etc/localtime (symlink unavailable)", zone)
				}
			}
		} else {
			report.note("timezone %q not available in this guest, left unchanged", zone)
		}
	} else {
		report.note("no timezone in profile, left unchanged")
	}

	// Locale.
	if p.Locale != "" {
		line := "LANG=" + p.Locale + "\n"
		_ = writeIfChanged(&report, at("/etc/locale.conf"), line, 0o644, opts.DryRun)
		if _, err := os.Stat("/etc/default"); err == nil {
			_ = writeIfChanged(&report, at("/etc/default/locale"), line, 0o644, opts.DryRun)
		}
	}

	// User account.
	if p.User.Name != "" {
		if offline && passwdHasUser(at("/etc/passwd"), p.User.Name) {
			report.note("user %q already exists in the staged rootfs", p.User.Name)
		} else if !offline {
			if _, err := osuser.Lookup(p.User.Name); err == nil {
				report.note("user %q already exists", p.User.Name)
			} else if !opts.CreateUser {
				report.note("user %q missing (pass --create-user)", p.User.Name)
			} else if opts.DryRun {
				report.note("would create user %q", p.User.Name)
			} else if err := runOptional(&report, "useradd", "-m", "-s", "/bin/bash", p.User.Name); err == nil {
				report.note("created user %q", p.User.Name)
			} else {
				report.note("create user %q failed: %v", p.User.Name, err)
			}
		} else if opts.CreateUser {
			if opts.DryRun {
				report.note("would create user %q", p.User.Name)
			} else if offline {
				if err := ensureUserEntry(&report, at("/etc/passwd"), p.User.Name, opts.DryRun); err != nil {
					return report, err
				}
			} else if err := runOptional(&report, "useradd", "-m", "-s", "/bin/bash", p.User.Name); err == nil {
				report.note("created user %q", p.User.Name)
			} else {
				report.note("create user %q failed: %v", p.User.Name, err)
			}
		} else {
			report.note("user %q missing (pass --create-user)", p.User.Name)
		}
	}

	// Keep /etc/hosts consistent with the new hostname.
	if err := ensureHostsLine(&report, p.Hostname, at("/etc/hosts"), opts.DryRun); err != nil {
		return report, err
	}
	return report, nil
}

func writeIfChanged(report *ApplyReport, path, content string, perm os.FileMode, dryRun bool) error {
	if current, err := os.ReadFile(path); err == nil && string(current) == content {
		report.note("%s already has the desired content", path)
		return nil
	}
	if dryRun {
		report.note("would write %s", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	report.note("wrote %s", path)
	return nil
}

func ensureHostsLine(report *ApplyReport, hostname, path string, dryRun bool) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		report.note("/etc/hosts not readable, skipped")
		return nil
	}
	if os.IsNotExist(err) {
		// A freshly staged rootfs has no hosts file yet; write a minimal one
		// instead of pointing the hostname nowhere.
		data = []byte("127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n")
		report.note("created a minimal /etc/hosts")
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[0] == "127.0.0.1" || fields[0] == "127.0.1.1") {
			rest := append([]string{fields[0], hostname}, fields[2:]...)
			if fields[1] != hostname {
				lines[i] = strings.Join(rest, " ")
				changed = true
			}
			break
		}
	}
	if !changed {
		report.note("/etc/hosts unchanged")
		return nil
	}
	if dryRun {
		report.note("would point 127.0.0.1 at %s in /etc/hosts", hostname)
		return nil
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("write /etc/hosts: %w", err)
	}
	report.note("updated /etc/hosts for %s", hostname)
	return nil
}

// etcZoneFromOffset converts a Windows UTC offset like "08:00:00" to the
// inverted-sign Etc zone POSIX uses ("Etc/GMT-8").
func etcZoneFromOffset(offset string) string {
	offset = strings.TrimSpace(offset)
	if offset == "" {
		return ""
	}
	negative := strings.HasPrefix(offset, "-")
	offset = strings.TrimPrefix(offset, "-")
	offset = strings.TrimPrefix(offset, "+")
	hours, _, _ := strings.Cut(offset, ":")
	n, err := strconv.Atoi(hours)
	if err != nil {
		return ""
	}
	if n == 0 {
		return "Etc/UTC"
	}
	if negative {
		return "Etc/GMT+" + strconv.Itoa(n)
	}
	return "Etc/GMT-" + strconv.Itoa(n)
}

// SanitizeHostname reports whether a hostname is safe to apply. Kept as the
// apply-side name for [ValidHostname].
func SanitizeHostname(h string) bool {
	return ValidHostname(h)
}

// runOptional runs a helper tool; a missing tool is reported so the caller
// can downgrade it to a note.
func runOptional(report *ApplyReport, name string, args ...string) error {
	_ = report
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	return exec.Command(path, args...).Run()
}

// passwdHasUser reports whether a staged passwd file already lists the user.
// The host's own user database is meaningless for a rootfs being built.
func passwdHasUser(path, name string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+":") {
			return true
		}
	}
	return false
}

// ensureUserEntry appends a passwd line when staging a rootfs, where useradd
// cannot run against the target system.
func ensureUserEntry(report *ApplyReport, passwdPath, name string, dryRun bool) error {
	data, _ := os.ReadFile(passwdPath)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+":") {
			report.note("user %q already in passwd", name)
			return nil
		}
	}
	line := name + ":x:1000:1000::/home/" + name + ":/bin/bash\n"
	if dryRun {
		report.note("would append user %q to passwd", name)
		return nil
	}
	f, err := os.OpenFile(passwdPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("append passwd: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("append passwd: %w", err)
	}
	report.note("appended user %q to passwd (offline)", name)
	return nil
}
