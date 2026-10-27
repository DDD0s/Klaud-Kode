package identity

import (
	"strings"
	"testing"
)

func TestMachineIDFromGUID(t *testing.T) {
	cases := map[string]string{
		"6F9619FF-8B86-D011-B42D-00C04FC964FF":   "6f9619ff8b86d011b42d00c04fc964ff",
		"{6F9619FF-8B86-D011-B42D-00C04FC964FF}": "6f9619ff8b86d011b42d00c04fc964ff",
		"not-a-guid":                             "",
		"":                                       "",
		"1234":                                   "",
	}
	for in, want := range cases {
		if got := MachineIDFromGUID(in); got != want {
			t.Errorf("MachineIDFromGUID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanPlaceholders(t *testing.T) {
	for _, bad := range []string{
		"To be filled by O.E.M.", "Default string", "None",
		"00000000-0000-0000-0000-000000000000", "0000000000", "FFFFFFFFFF",
	} {
		if got := Clean(bad); got != "" {
			t.Errorf("Clean(%q) = %q, want empty", bad, got)
		}
	}
	if got := Clean("  XPS 15 9530 "); got != "XPS 15 9530" {
		t.Errorf("Clean kept value mangled: %q", got)
	}
	if got := Clean("C9XYZ123"); got != "C9XYZ123" {
		t.Errorf("Clean dropped a real serial: %q", got)
	}
}

func TestIANAFromWindows(t *testing.T) {
	if got := IANAFromWindows("China Standard Time"); got != "Asia/Shanghai" {
		t.Errorf("got %q", got)
	}
	if got := IANAFromWindows("No Such Zone"); got != "" {
		t.Errorf("unknown zone = %q, want empty", got)
	}
}

func TestEtcZoneFromOffset(t *testing.T) {
	cases := map[string]string{
		"08:00:00":  "Etc/GMT-8",
		"-05:00:00": "Etc/GMT+5",
		"+09:00:00": "Etc/GMT-9",
		"00:00:00":  "Etc/UTC",
		"":          "",
		"garbage":   "",
	}
	for in, want := range cases {
		if got := etcZoneFromOffset(in); got != want {
			t.Errorf("etcZoneFromOffset(%q) = %q, want %q", in, got, want)
		}
	}
}

func sampleProfile() Profile {
	return Profile{
		Schema:     Schema,
		SourceOS:   "windows",
		Hostname:   "DESKTOP-TEST01",
		MachineID:  "6F9619FF-8B86-D011-B42D-00C04FC964FF",
		SystemUUID: "4c4c4544-004d-4210-8033-b3c04f503631",
		SMBIOS: SMBIOS{
			Manufacturer: "Dell Inc.",
			Product:      "XPS 15 9530",
			Serial:       "AB12CD3",
			Family:       "XPS",
		},
		CPU:      CPU{Model: "Intel Core i7, test", Cores: 8, Threads: 16},
		MemoryMB: 32768,
		Timezone: Timezone{Windows: "China Standard Time"},
		Locale:   "zh-CN.UTF-8",
		User:     User{Name: "alice"},
	}
}

func TestValidateNormalizes(t *testing.T) {
	p := sampleProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.MachineID != "6f9619ff8b86d011b42d00c04fc964ff" {
		t.Errorf("machine id not normalized: %q", p.MachineID)
	}
	if p.Timezone.IANA != "Asia/Shanghai" {
		t.Errorf("timezone not mapped: %+v", p.Timezone)
	}
	bad := sampleProfile()
	bad.MachineID = "nope"
	if err := bad.Validate(); err == nil {
		t.Error("expected validation error for unusable machine id")
	}
}

func TestParseProfileRoundTrip(t *testing.T) {
	p := sampleProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseProfile(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != p.Hostname || got.MachineID != p.MachineID {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestQEMUIdentityArgs(t *testing.T) {
	p := sampleProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	args := QEMUIdentityArgs(p)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-uuid 4c4c4544-004d-4210-8033-b3c04f503631",
		"manufacturer=Dell Inc.",
		"product=XPS 15 9530",
		"serial=AB12CD3",
		"-smp 16",
		"-m 32768",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	// commas inside values must not break QEMU's option parsing
	if strings.Contains(joined, "i7, test") {
		t.Errorf("comma not sanitised: %s", joined)
	}
	if smbiosValue("a,b  c") != "a b c" {
		t.Errorf("smbiosValue = %q", smbiosValue("a,b  c"))
	}
}

func TestGuestEnv(t *testing.T) {
	p := sampleProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	env := GuestEnv(p)
	for _, want := range []string{"KL_IDENTITY_HOSTNAME='DESKTOP-TEST01'", "KL_IDENTITY_MACHINE_ID='6f9619ff8b86d011b42d00c04fc964ff'", "KL_IDENTITY_TZ='Asia/Shanghai'"} {
		if !strings.Contains(env, want) {
			t.Errorf("guest env missing %q:\n%s", want, env)
		}
	}
}

// A crafted profile must not be able to inject lines into /etc/hostname or
// /etc/hosts through the hostname.
func TestValidateRejectsUnsafeHostname(t *testing.T) {
	for _, name := range []string{
		"evil\n127.0.0.1 attacker",
		"host name",
		"host/name",
		"-leading-dash",
		"host_name",
		strings.Repeat("a", 64),
	} {
		p := sampleProfile()
		p.Hostname = name
		if err := p.Validate(); err == nil {
			t.Errorf("Validate accepted unsafe hostname %q", name)
		}
	}
	p := sampleProfile()
	p.Hostname = "Valid-Host-01"
	if err := p.Validate(); err != nil {
		t.Errorf("valid hostname rejected: %v", err)
	}
}
