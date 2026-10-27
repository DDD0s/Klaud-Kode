package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stage must describe the captured computer and nothing else: every file
// the CLI can read about itself comes from the profile.
func TestBuildStageMaterialisesProfile(t *testing.T) {
	root := t.TempDir()
	p := sampleProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	report, err := BuildStage(p, StageOptions{Rootfs: root})
	if err != nil {
		t.Fatalf("stage: %v\n%s", err, report.String())
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
	if got := read("etc/timezone"); got != "Asia/Shanghai\n" {
		t.Errorf("timezone = %q", got)
	}
	if got := read("sys/class/dmi/id/product_name"); got != "XPS 15 9530\n" {
		t.Errorf("product_name = %q", got)
	}
	if got := read("proc/cpuinfo"); !strings.Contains(got, "Intel Core i7, test") {
		t.Errorf("cpuinfo missing model: %.80q", got)
	}
	if got := read("stage.env"); !strings.Contains(got, "HOSTNAME=DESKTOP-TEST01") || !strings.Contains(got, "HOME=/home/alice") {
		t.Errorf("stage.env wrong:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "home", "alice")); err != nil {
		t.Errorf("staged home missing: %v", err)
	}
}

// Placeholder-stripped firmware fields must stay absent rather than being
// replaced by invented values.
func TestPlanSkipsUnusableFirmware(t *testing.T) {
	p := sampleProfile()
	p.SMBIOS.Product = "System Product Name"
	p.SMBIOS.Serial = "System Serial Number"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, f := range Plan(p) {
		if f.Path == "sys/class/dmi/id/product_name" && !strings.Contains(f.Content, "System") {
			t.Errorf("placeholder product leaked: %q", f.Content)
		}
		if f.Path == "sys/class/dmi/id/product_serial" {
			t.Errorf("placeholder serial staged: %q", f.Content)
		}
	}
}

func TestStageRefusesRoot(t *testing.T) {
	p := sampleProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildStage(p, StageOptions{Rootfs: "/"}); err == nil {
		t.Error("expected refusal to stage over /")
	}
	// A dry run writes nothing.
	root := t.TempDir()
	if _, err := BuildStage(p, StageOptions{Rootfs: root, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "hostname")); !os.IsNotExist(err) {
		t.Error("dry run wrote files")
	}
}

func TestCPUInfoThreadCount(t *testing.T) {
	p := sampleProfile()
	got := cpuinfoContent(p)
	if n := strings.Count(got, "processor\t:"); n != 16 {
		t.Errorf("processors = %d, want 16", n)
	}
}
