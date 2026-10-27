package identity

import (
	"os"
	"path/filepath"
	"testing"
)

// Apply into a staged rootfs must produce the guest identity without touching
// the host system.
func TestApplyIntoRootPrefix(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr", "share", "zoneinfo", "Asia"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr", "share", "zoneinfo", "Asia", "Shanghai"), []byte("TZif"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc-hosts-seed"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := sampleProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	report, err := Apply(p, ApplyOptions{RootPrefix: root, CreateUser: true})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, report.String())
	}
	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(data)
	}
	if got := read("etc/hostname"); got != "DESKTOP-TEST01\n" {
		t.Errorf("hostname = %q", got)
	}
	if got := read("etc/machine-id"); got != p.MachineID+"\n" {
		t.Errorf("machine-id = %q", got)
	}
	// The target may be a symlink, or a copy where symlinking needs privilege.
	if link, err := os.Readlink(filepath.Join(root, "etc", "localtime")); err == nil {
		if link != filepath.Join(root, "usr", "share", "zoneinfo", "Asia", "Shanghai") {
			t.Errorf("localtime link = %q", link)
		}
	} else if got := read("etc/localtime"); got != "TZif" {
		t.Errorf("localtime copy = %q", got)
	}
	if got := read("etc/locale.conf"); got != "LANG=zh-CN.UTF-8\n" {
		t.Errorf("locale.conf = %q", got)
	}
	if got := read("etc/passwd"); !contains(got, "alice:x:1000:1000") {
		t.Errorf("passwd missing user: %q", got)
	}
	if got := read("etc/hosts"); !contains(got, "127.0.0.1 DESKTOP-TEST01") {
		t.Errorf("hosts not updated: %q", got)
	}
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	p := sampleProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	report, err := Apply(p, ApplyOptions{RootPrefix: root, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run apply: %v\n%s", err, report.String())
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "hostname")); !os.IsNotExist(err) {
		t.Error("dry run wrote files")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
