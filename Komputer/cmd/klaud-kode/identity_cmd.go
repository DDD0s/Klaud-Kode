package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/identity"
)

// runIdentity implements: klaud-kode identity <capture|apply|qemu|stage|show> [flags]
//
// capture runs on the caller's own computer and records its machine identity.
// apply runs on the brain (Linux guest) and makes that guest report the
// captured identity. qemu prints the QEMU arguments and guest.env seed lines
// for the same profile.
func runIdentity(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: klaud-kode identity <capture|apply|qemu|stage|show> [flags]")
	}
	switch args[0] {
	case "capture":
		return identityCapture(args[1:])
	case "apply":
		return identityApply(args[1:])
	case "stage":
		return identityStage(args[1:])
	case "qemu":
		return identityQEMU(args[1:])
	case "show":
		return identityShow(args[1:])
	default:
		return fmt.Errorf("unknown identity subcommand %q", args[0])
	}
}

func identityFlags(args []string) map[string]string {
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name := strings.TrimPrefix(a, "--")
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			flags[name] = args[i+1]
			i++
		} else {
			flags[name] = "true"
		}
	}
	return flags
}

// flagValue reads a valued flag; the boolean placeholder "true" means the
// operator forgot the value, which must not become a filename.
func flagValue(flags map[string]string, name string) (string, error) {
	v, present := flags[name]
	if !present {
		return "", nil
	}
	if v == "true" {
		return "", fmt.Errorf("--%s needs a value", name)
	}
	return v, nil
}

func loadProfile(path string) (identity.Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return identity.Profile{}, err
	}
	return identity.ParseProfile(data)
}

func identityCapture(args []string) error {
	flags := identityFlags(args)
	profile, err := identity.Capture()
	if err != nil {
		return err
	}
	data, err := profile.Marshal()
	if err != nil {
		return err
	}
	if flags["stdout"] == "true" {
		fmt.Println(string(data))
		return nil
	}
	out, err := flagValue(flags, "out")
	if err != nil {
		return err
	}
	if out == "" {
		out = "machine-identity.json"
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "captured %s (%s) -> %s\n", profile.Hostname, profile.SourceOS, out)
	fmt.Fprintln(os.Stderr, "note: this file contains real hardware serials; treat it like a credential")
	return nil
}

func identityApply(args []string) error {
	flags := identityFlags(args)
	path, err := flagValue(flags, "profile")
	if err != nil {
		return err
	}
	if path == "" {
		path = "machine-identity.json"
	}
	profile, err := loadProfile(path)
	if err != nil {
		return err
	}
	root, err := flagValue(flags, "root")
	if err != nil {
		return err
	}
	report, err := identity.Apply(profile, identity.ApplyOptions{
		DryRun:     flags["dry-run"] == "true",
		CreateUser: flags["create-user"] == "true",
		RootPrefix: root,
	})
	if report.String() != "" {
		fmt.Println(report.String())
	}
	return err
}

// identityStage materialises the fake system the brain-side CLI will run in:
// every file it can read about itself describes the caller's computer.
func identityStage(args []string) error {
	flags := identityFlags(args)
	root, err := flagValue(flags, "root")
	if err != nil {
		return err
	}
	path, err := flagValue(flags, "profile")
	if err != nil {
		return err
	}
	if path == "" {
		path = "machine-identity.json"
	}
	profile, err := loadProfile(path)
	if err != nil {
		return err
	}
	if root == "" {
		return fmt.Errorf("stage needs --root <dir>")
	}
	report, err := identity.BuildStage(profile, identity.StageOptions{
		Rootfs: root,
		DryRun: flags["dry-run"] == "true",
	})
	if report.String() != "" {
		fmt.Println(report.String())
	}
	return err
}

func identityQEMU(args []string) error {
	flags := identityFlags(args)
	path, err := flagValue(flags, "profile")
	if err != nil {
		return err
	}
	if path == "" {
		path = "machine-identity.json"
	}
	profile, err := loadProfile(path)
	if err != nil {
		return err
	}
	if flags["env-only"] != "true" {
		fmt.Println(strings.Join(identity.QEMUIdentityArgs(profile), " "))
	}
	if flags["args-only"] != "true" {
		fmt.Print("# ---- guest.env ----\n")
		fmt.Print(identity.GuestEnv(profile))
	}
	return nil
}

func identityShow(args []string) error {
	flags := identityFlags(args)
	path, err := flagValue(flags, "profile")
	if err != nil {
		return err
	}
	if path == "" {
		path = "machine-identity.json"
	}
	profile, err := loadProfile(path)
	if err != nil {
		return err
	}
	masked := func(s string) string {
		if s == "" {
			return "(none)"
		}
		if len(s) <= 6 {
			return "*** (" + fmt.Sprint(len(s)) + " chars)"
		}
		return s[:3] + "..." + s[len(s)-2:] + " (" + fmt.Sprint(len(s)) + " chars)"
	}
	nic := "(none)"
	if n, ok := profile.PrimaryNIC(); ok {
		nic = n.MAC
	}
	fmt.Printf("hostname:    %s\n", profile.Hostname)
	fmt.Printf("source OS:   %s (%s/%s)\n", profile.SourceOS, profile.OS.Name, profile.OS.Arch)
	fmt.Printf("machine-id:  %s\n", masked(profile.MachineID))
	fmt.Printf("system uuid: %s\n", masked(profile.SystemUUID))
	fmt.Printf("smbios:      %s %s (serial %s)\n", profile.SMBIOS.Manufacturer, profile.SMBIOS.Product, masked(profile.SMBIOS.Serial))
	fmt.Printf("cpu:         %s (%d cores / %d threads)\n", profile.CPU.Model, profile.CPU.Cores, profile.CPU.Threads)
	fmt.Printf("memory MB:   %d\n", profile.MemoryMB)
	fmt.Printf("nic:         %s\n", nic)
	fmt.Printf("timezone:    %s (windows: %s, offset %s)\n", profile.Timezone.IANA, profile.Timezone.Windows, profile.Timezone.UTCOffset)
	fmt.Printf("locale:      %s\n", profile.Locale)
	fmt.Printf("user:        %s (home %s)\n", profile.User.Name, profile.User.Home)
	fmt.Printf("disks:       %d, nics: %d, gpus: %d\n", len(profile.Disks), len(profile.NICs), len(profile.GPUs))
	return nil
}
