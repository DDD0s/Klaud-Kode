// Package identity captures the real machine identity of the caller's computer
// and applies it to the brain-side environment, so the environment the CLI runs
// in reports the caller's computer instead of the Linux server it sits on.
// Egress is deliberately out of scope: the brain keeps its own network exit.
package identity

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Schema is the on-disk profile version. Bump on any incompatible field change.
const Schema = "kloud-kode/machine-identity/1"

// Profile is one real machine's identity, captured on that machine and applied
// on the brain. It carries hardware serials, so treat it as a secret file.
type Profile struct {
	Schema     string   `json:"schema"`
	CapturedAt string   `json:"capturedAt"`
	SourceOS   string   `json:"sourceOS"`
	Hostname   string   `json:"hostname"`
	MachineID  string   `json:"machineId"`
	SystemUUID string   `json:"systemUuid,omitempty"`
	SMBIOS     SMBIOS   `json:"smbios"`
	CPU        CPU      `json:"cpu"`
	MemoryMB   int      `json:"memoryMb"`
	Disks      []Disk   `json:"disks,omitempty"`
	NICs       []NIC    `json:"nics,omitempty"`
	GPUs       []GPU    `json:"gpus,omitempty"`
	OS         OS       `json:"os"`
	Timezone   Timezone `json:"timezone"`
	Locale     string   `json:"locale"`
	User       User     `json:"user"`
}

// SMBIOS carries the system firmware identity (DMI type 1 and baseboard).
type SMBIOS struct {
	Manufacturer      string `json:"manufacturer,omitempty"`
	Product           string `json:"product,omitempty"`
	Version           string `json:"version,omitempty"`
	Serial            string `json:"serial,omitempty"`
	Family            string `json:"family,omitempty"`
	SKU               string `json:"sku,omitempty"`
	BoardManufacturer string `json:"boardManufacturer,omitempty"`
	BoardProduct      string `json:"boardProduct,omitempty"`
	BoardSerial       string `json:"boardSerial,omitempty"`
}

// CPU describes the processor as the guest should present it.
type CPU struct {
	Model   string `json:"model,omitempty"`
	Vendor  string `json:"vendor,omitempty"`
	Cores   int    `json:"cores,omitempty"`
	Threads int    `json:"threads,omitempty"`
}

// Disk is one non-removable storage device.
type Disk struct {
	Model  string `json:"model,omitempty"`
	Serial string `json:"serial,omitempty"`
	SizeGB int    `json:"sizeGb,omitempty"`
}

// NIC is one network interface.
type NIC struct {
	MAC         string `json:"mac"`
	Description string `json:"description,omitempty"`
}

// GPU is one display adapter.
type GPU struct {
	Name string `json:"name"`
}

// OS is the operating system the identity was captured from.
type OS struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Build   string `json:"build,omitempty"`
	Arch    string `json:"arch,omitempty"`
}

// Timezone carries both the Windows name (as captured) and the IANA name the
// Linux guest needs, plus the UTC offset as a fallback for unmapped zones.
type Timezone struct {
	IANA      string `json:"iana,omitempty"`
	Windows   string `json:"windowsName,omitempty"`
	UTCOffset string `json:"utcOffset,omitempty"`
}

// User is the account the identity belongs to.
type User struct {
	Name  string `json:"name,omitempty"`
	Home  string `json:"home,omitempty"`
	Shell string `json:"shell,omitempty"`
}

var hex32Re = regexp.MustCompile(`^[0-9a-f]{32}$`)

// placeholderValues are firmware strings that carry no identity. Passing them
// to a guest is worse than leaving the field empty: they are shared by every
// machine whose firmware was never populated.
var placeholderValues = map[string]bool{
	"":                                     true,
	"to be filled by o.e.m.":               true,
	"to be filled by oem":                  true,
	"default string":                       true,
	"none":                                 true,
	"not applicable":                       true,
	"not specified":                        true,
	"system serial number":                 true,
	"system product name":                  true,
	"serialnumber":                         true,
	"chassis serial number":                true,
	"base board serial number":             true,
	"00000000-0000-0000-0000-000000000000": true,
	"ffffffff-ffff-ffff-ffff-ffffffffffff": true,
}

// Clean drops a value that does not identify a machine.
func Clean(v string) string {
	v = strings.TrimSpace(v)
	if placeholderValues[strings.ToLower(v)] {
		return ""
	}
	// All-zero or all-F serials carry no identity either.
	compact := strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ':
			return -1
		}
		return r
	}, v)
	if compact != "" && (strings.Trim(compact, "0") == "" || strings.Trim(strings.ToLower(compact), "f") == "") {
		return ""
	}
	return v
}

// MachineIDFromGUID converts a Windows MachineGuid (or any GUID) to the
// 32-hex form Linux expects in /etc/machine-id.
func MachineIDFromGUID(guid string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(guid) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	if b.Len() != 32 {
		return ""
	}
	return b.String()
}

// NormalizeMachineID accepts a value already in Linux machine-id form.
func NormalizeMachineID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if hex32Re.MatchString(s) {
		return s
	}
	return MachineIDFromGUID(s)
}

// windowsToIANA maps the common Windows time zone ids to IANA names. Unmapped
// zones fall back to the UTC offset at apply time.
var windowsToIANA = map[string]string{
	"China Standard Time":        "Asia/Shanghai",
	"TAB Standard Time":          "Asia/Taipei",
	"Taipei Standard Time":       "Asia/Taipei",
	"Tokyo Standard Time":        "Asia/Tokyo",
	"Korea Standard Time":        "Asia/Seoul",
	"Singapore Standard Time":    "Asia/Singapore",
	"SE Asia Standard Time":      "Asia/Bangkok",
	"India Standard Time":        "Asia/Kolkata",
	"AUS Eastern Standard Time":  "Australia/Sydney",
	"UTC":                        "UTC",
	"GMT Standard Time":          "Europe/London",
	"W. Europe Standard Time":    "Europe/Berlin",
	"Eastern Standard Time":      "America/New_York",
	"Central Standard Time":      "America/Chicago",
	"Mountain Standard Time":     "America/Denver",
	"Pacific Standard Time":      "America/Los_Angeles",
	"Alaskan Standard Time":      "America/Anchorage",
	"Hawaiian Standard Time":     "Pacific/Honolulu",
	"New Zealand Standard Time":  "Pacific/Auckland",
	"Russian Standard Time":      "Europe/Moscow",
	"Middle East Standard Time":  "Asia/Beirut",
	"Arabian Standard Time":      "Asia/Dubai",
	"Sri Lanka Standard Time":    "Asia/Colombo",
	"Myanmar Standard Time":      "Asia/Yangon",
	"West Asia Standard Time":    "Asia/Almaty",
	"Central Asia Standard Time": "Asia/Almaty",
}

// IANAFromWindows returns the IANA name for a Windows zone id, or "".
func IANAFromWindows(name string) string {
	return windowsToIANA[strings.TrimSpace(name)]
}

// Validate checks the fields the applier depends on and sanitises the rest.
func (p *Profile) Validate() error {
	if p.Schema != Schema {
		return fmt.Errorf("profile schema %q, want %q", p.Schema, Schema)
	}
	p.Hostname = strings.TrimSpace(p.Hostname)
	if p.Hostname == "" {
		return fmt.Errorf("profile has no hostname")
	}
	if !ValidHostname(p.Hostname) {
		return fmt.Errorf("profile hostname %q is not a safe hostname", p.Hostname)
	}
	p.MachineID = NormalizeMachineID(p.MachineID)
	if p.MachineID == "" {
		return fmt.Errorf("profile has no usable machine id")
	}
	p.SMBIOS.Manufacturer = Clean(p.SMBIOS.Manufacturer)
	p.SMBIOS.Product = Clean(p.SMBIOS.Product)
	p.SMBIOS.Version = Clean(p.SMBIOS.Version)
	p.SMBIOS.Serial = Clean(p.SMBIOS.Serial)
	p.SMBIOS.Family = Clean(p.SMBIOS.Family)
	p.SMBIOS.SKU = Clean(p.SMBIOS.SKU)
	p.SMBIOS.BoardManufacturer = Clean(p.SMBIOS.BoardManufacturer)
	p.SMBIOS.BoardProduct = Clean(p.SMBIOS.BoardProduct)
	p.SMBIOS.BoardSerial = Clean(p.SMBIOS.BoardSerial)
	for i := range p.Disks {
		p.Disks[i].Serial = Clean(p.Disks[i].Serial)
	}
	if p.Timezone.IANA == "" && p.Timezone.Windows != "" {
		p.Timezone.IANA = IANAFromWindows(p.Timezone.Windows)
	}
	return nil
}

// PrimaryNIC returns the first NIC with a MAC address.
func (p *Profile) PrimaryNIC() (NIC, bool) {
	for _, n := range p.NICs {
		if strings.TrimSpace(n.MAC) != "" {
			return n, true
		}
	}
	return NIC{}, false
}

// ValidHostname reports whether h can be written to /etc/hostname and used in
// /etc/hosts without injecting lines: letters, digits, hyphens and dots only,
// no leading/trailing hyphen or dot, no empty label, at most 63 chars.
func ValidHostname(h string) bool {
	if h == "" || len(h) > 63 || h[0] == '-' || h[0] == '.' {
		return false
	}
	prevDot := false
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			prevDot = false
		case r == '-':
			if prevDot {
				return false
			}
		case r == '.':
			if prevDot {
				return false
			}
			prevDot = true
		default:
			return false
		}
	}
	return !prevDot && h[len(h)-1] != '-'
}

// Marshal renders the profile as indented JSON.
func (p Profile) Marshal() ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}

// ParseProfile reads and validates a profile document.
func ParseProfile(data []byte) (Profile, error) {
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}
