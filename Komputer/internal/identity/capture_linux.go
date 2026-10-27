//go:build linux

package identity

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func capturePlatform(p *Profile) error {
	p.MachineID = NormalizeMachineID(readTrim("/etc/machine-id"))
	p.SystemUUID = readTrim("/sys/class/dmi/id/product_uuid")
	p.SMBIOS = SMBIOS{
		Manufacturer:      readTrim("/sys/class/dmi/id/sys_vendor"),
		Product:           readTrim("/sys/class/dmi/id/product_name"),
		Version:           readTrim("/sys/class/dmi/id/product_version"),
		Serial:            readTrim("/sys/class/dmi/id/product_serial"),
		Family:            readTrim("/sys/class/dmi/id/product_family"),
		SKU:               readTrim("/sys/class/dmi/id/product_sku"),
		BoardManufacturer: readTrim("/sys/class/dmi/id/board_vendor"),
		BoardProduct:      readTrim("/sys/class/dmi/id/board_name"),
		BoardSerial:       readTrim("/sys/class/dmi/id/board_serial"),
	}
	cpuinfo := readTrim("/proc/cpuinfo")
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name":
			if p.CPU.Model == "" {
				p.CPU.Model = strings.TrimSpace(value)
			}
		case "vendor_id":
			if p.CPU.Vendor == "" {
				p.CPU.Vendor = strings.TrimSpace(value)
			}
		case "cpu cores":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				p.CPU.Cores = n
			}
		}
	}
	if meminfo := readTrim("/proc/meminfo"); meminfo != "" {
		for _, line := range strings.Split(meminfo, "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.Atoi(fields[1]); err == nil {
						p.MemoryMB = kb / 1024
					}
				}
				break
			}
		}
	}
	osRelease := readTrim("/etc/os-release")
	for _, line := range strings.Split(osRelease, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, "\"")
		switch key {
		case "PRETTY_NAME":
			p.OS.Name = value
		case "VERSION_ID":
			p.OS.Version = value
		}
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.LastIndex(link, "zoneinfo/"); i >= 0 {
			p.Timezone.IANA = link[i+len("zoneinfo/"):]
		}
	}
	if lang := os.Getenv("LANG"); lang != "" {
		p.Locale = lang
	}
	p.Disks = readDisks()
	return nil
}

func readDisks() []Disk {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil
	}
	var disks []Disk
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		base := filepath.Join("/sys/block", name)
		disk := Disk{
			Model:  readTrim(filepath.Join(base, "device/model")),
			Serial: readTrim(filepath.Join(base, "device/serial")),
		}
		if sectors := readTrim(filepath.Join(base, "size")); sectors != "" {
			if n, err := strconv.ParseInt(sectors, 10, 64); err == nil {
				disk.SizeGB = int(n * 512 / 1_000_000_000)
			}
		}
		if disk.Model != "" || disk.SizeGB > 0 {
			disks = append(disks, disk)
		}
	}
	return disks
}
