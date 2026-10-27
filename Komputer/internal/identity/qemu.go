package identity

import (
	"strconv"
	"strings"
)

// smbiosValue makes a string safe to place in a comma-separated QEMU option.
func smbiosValue(s string) string {
	s = strings.ReplaceAll(s, ",", " ")
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// QEMUIdentityArgs builds the identity-only QEMU arguments for a profile:
// machine UUID, SMBIOS identity, vCPU count and memory. Device wiring (drive
// serials, NIC MAC) is deployment-specific, so those values are exported in
// GuestEnv for the operator to splice into their existing drive/netdev lines.
// The CPU model string cannot be reproduced by a named QEMU CPU model; pass
// -cpu host on a matching host if exact model presence matters.
func QEMUIdentityArgs(p Profile) []string {
	var args []string
	if p.SystemUUID != "" {
		args = append(args, "-uuid", p.SystemUUID)
	}
	s1 := []string{"type=1"}
	if v := smbiosValue(p.SMBIOS.Manufacturer); v != "" {
		s1 = append(s1, "manufacturer="+v)
	}
	if v := smbiosValue(p.SMBIOS.Product); v != "" {
		s1 = append(s1, "product="+v)
	}
	if v := smbiosValue(p.SMBIOS.Version); v != "" {
		s1 = append(s1, "version="+v)
	}
	if v := smbiosValue(p.SMBIOS.Serial); v != "" {
		s1 = append(s1, "serial="+v)
	}
	if p.SystemUUID != "" {
		s1 = append(s1, "uuid="+p.SystemUUID)
	}
	if v := smbiosValue(p.SMBIOS.Family); v != "" {
		s1 = append(s1, "family="+v)
	}
	if v := smbiosValue(p.SMBIOS.SKU); v != "" {
		s1 = append(s1, "sku="+v)
	}
	if len(s1) > 1 {
		args = append(args, "-smbios", strings.Join(s1, ","))
	}
	s2 := []string{"type=2"}
	if v := smbiosValue(p.SMBIOS.BoardManufacturer); v != "" {
		s2 = append(s2, "manufacturer="+v)
	}
	if v := smbiosValue(p.SMBIOS.BoardProduct); v != "" {
		s2 = append(s2, "product="+v)
	}
	if v := smbiosValue(p.SMBIOS.BoardSerial); v != "" {
		s2 = append(s2, "serial="+v)
	}
	if len(s2) > 1 {
		args = append(args, "-smbios", strings.Join(s2, ","))
	}
	if p.CPU.Threads > 0 {
		args = append(args, "-smp", strconv.Itoa(p.CPU.Threads))
	}
	if p.MemoryMB > 0 {
		args = append(args, "-m", strconv.Itoa(p.MemoryMB))
	}
	return args
}

// GuestEnv renders the seed variables the guest-side applier (or an operator)
// consumes. Values are shell-quoted by construction: they contain no spaces
// after cleaning, except CPU model which is informational only.
func GuestEnv(p Profile) string {
	var lines []string
	put := func(key, value string) {
		if value != "" {
			lines = append(lines, key+"='"+strings.ReplaceAll(value, "'", "")+"'")
		}
	}
	put("KL_IDENTITY_HOSTNAME", p.Hostname)
	put("KL_IDENTITY_MACHINE_ID", p.MachineID)
	put("KL_IDENTITY_UUID", p.SystemUUID)
	put("KL_IDENTITY_TZ", p.Timezone.IANA)
	put("KL_IDENTITY_LANG", p.Locale)
	put("KL_IDENTITY_USER", p.User.Name)
	put("KL_IDENTITY_CPU_MODEL", p.CPU.Model)
	if nic, ok := p.PrimaryNIC(); ok {
		put("KL_IDENTITY_NIC_MAC", nic.MAC)
	}
	if len(p.Disks) > 0 {
		put("KL_IDENTITY_DISK_SERIAL", p.Disks[0].Serial)
		put("KL_IDENTITY_DISK_MODEL", p.Disks[0].Model)
	}
	return strings.Join(lines, "\n") + "\n"
}
