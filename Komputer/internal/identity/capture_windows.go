//go:build windows

package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"
)

// One PowerShell round-trip gathers everything CIM exposes; a missing class
// yields empty fields rather than failing the whole capture.
const captureScript = `
$ErrorActionPreference = 'SilentlyContinue'
function One($cls) { return @(Get-CimInstance $cls)[0] }
$cs = One 'Win32_ComputerSystem'
$prod = One 'Win32_ComputerSystemProduct'
$board = One 'Win32_BaseBoard'
$cpu = One 'Win32_Processor'
$os = One 'Win32_OperatingSystem'
$disks = @(Get-CimInstance Win32_DiskDrive) | ForEach-Object {
  [ordered]@{ model = "$($_.Model)"; serial = "$($_.SerialNumber)"; sizeGb = [math]::Round($_.Size / 1GB) }
}
$gpus = @(Get-CimInstance Win32_VideoController) | ForEach-Object { [ordered]@{ name = "$($_.Name)" } }
$tz = [System.TimeZoneInfo]::Local
$out = [ordered]@{
  machineGuid  = [string](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography' -Name MachineGuid).MachineGuid
  systemUuid   = "$($prod.UUID)"
  manufacturer = "$($prod.Vendor)"
  product      = "$($prod.Name)"
  sku          = "$($prod.SKUNumber)"
  sysSerial    = "$($prod.IdentifyingNumber)"
  boardVendor  = "$($board.Manufacturer)"
  boardProduct = "$($board.Product)"
  boardSerial  = "$($board.SerialNumber)"
  cpuModel     = "$($cpu.Name)"
  cpuVendor    = "$($cpu.Manufacturer)"
  cpuCores     = [int]$cpu.NumberOfCores
  osName       = "$($os.Caption)"
  osVersion    = "$($os.Version)"
  osBuild      = "$($os.BuildNumber)"
  osArch       = "$($os.OSArchitecture)"
  memoryKb     = [double]$os.TotalVisibleMemorySize
  timezone     = "$($tz.Id)"
  utcOffset    = $tz.BaseUtcOffset.ToString()
  locale       = [string][System.Globalization.CultureInfo]::CurrentUICulture.Name
  disks        = $disks
  gpus         = $gpus
}
[Console]::Out.Write(($out | ConvertTo-Json -Compress -Depth 4))
`

type winFacts struct {
	MachineGuid  string  `json:"machineGuid"`
	SystemUUID   string  `json:"systemUuid"`
	Manufacturer string  `json:"manufacturer"`
	Product      string  `json:"product"`
	SKU          string  `json:"sku"`
	SysSerial    string  `json:"sysSerial"`
	BoardVendor  string  `json:"boardVendor"`
	BoardProduct string  `json:"boardProduct"`
	BoardSerial  string  `json:"boardSerial"`
	CPUModel     string  `json:"cpuModel"`
	CPUVendor    string  `json:"cpuVendor"`
	CPUCores     int     `json:"cpuCores"`
	OSName       string  `json:"osName"`
	OSVersion    string  `json:"osVersion"`
	OSBuild      string  `json:"osBuild"`
	OSArch       string  `json:"osArch"`
	MemoryKB     float64 `json:"memoryKb"`
	Timezone     string  `json:"timezone"`
	UTCOffset    string  `json:"utcOffset"`
	Locale       string  `json:"locale"`
	Disks        []Disk  `json:"disks"`
	GPUs         []GPU   `json:"gpus"`
}

// encodePowerShell renders a script as the UTF-16LE base64 -EncodedCommand wants.
func encodePowerShell(script string) string {
	runes := utf16.Encode([]rune(script))
	buf := make([]byte, 0, len(runes)*2)
	for _, r := range runes {
		buf = append(buf, byte(r), byte(r>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// machineGUIDFallback reads only the registry value, for hosts where the full
// CIM pass fails.
func machineGUIDFallback(ctx context.Context) string {
	const script = `(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography' -Name MachineGuid).MachineGuid`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodePowerShell(script))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func capturePlatform(p *Profile) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// EncodedCommand runs the script as one unit. Feeding it on stdin makes
	// PowerShell evaluate line by line, which splits multi-line statements.
	encoded := encodePowerShell(captureScript)
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	var f winFacts
	if runErr == nil && len(bytes.TrimSpace(stdout.Bytes())) > 0 {
		// A CIM class that is missing on some hosts must not lose the whole
		// capture; whatever parsed is still useful.
		_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &f)
	}
	if f.MachineGuid == "" {
		// Fall back to a minimal query so a partial CIM failure cannot remove
		// the one field the profile cannot do without.
		f.MachineGuid = machineGUIDFallback(ctx)
	}
	if f.MachineGuid == "" && f.SystemUUID == "" {
		if runErr != nil {
			return fmt.Errorf("powershell: %v: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("no machine identity available from this host")
	}
	p.MachineID = MachineIDFromGUID(f.MachineGuid)
	p.SystemUUID = f.SystemUUID
	p.SMBIOS = SMBIOS{
		Manufacturer:      f.Manufacturer,
		Product:           f.Product,
		Serial:            f.SysSerial,
		SKU:               f.SKU,
		BoardManufacturer: f.BoardVendor,
		BoardProduct:      f.BoardProduct,
		BoardSerial:       f.BoardSerial,
	}
	p.CPU.Model = strings.TrimSpace(f.CPUModel)
	p.CPU.Vendor = f.CPUVendor
	p.CPU.Cores = f.CPUCores
	p.OS.Name = f.OSName
	p.OS.Version = f.OSVersion
	p.OS.Build = f.OSBuild
	if f.OSArch != "" {
		p.OS.Arch = mapArch(f.OSArch)
	}
	if f.MemoryKB > 0 {
		p.MemoryMB = int(f.MemoryKB / 1024)
	}
	p.Timezone.Windows = f.Timezone
	p.Timezone.IANA = IANAFromWindows(f.Timezone)
	p.Timezone.UTCOffset = f.UTCOffset
	p.Locale = localeFromCulture(f.Locale)
	p.Disks = f.Disks
	p.GPUs = f.GPUs
	return nil
}

func mapArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "64-bit", "64 bit", "amd64", "x64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "32-bit", "32 bit", "x86":
		return "386"
	default:
		return arch
	}
}
