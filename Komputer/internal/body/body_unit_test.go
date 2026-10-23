package body

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestDecodeOutputFallbacks(t *testing.T) {
	sjis, _ := hex.DecodeString("82b182f1834f815b82c982a282a4815b")
	if got := DecodeOutput(sjis, "shift_jis"); got.Text != "こんグーにいうー" || got.Encoding != "shift_jis" {
		t.Fatalf("shift_jis: %+v", got)
	}
	invalid := []byte{0xff, 0xfe, 0x41}
	if got := DecodeOutput(invalid, "utf-8"); got.Encoding != "latin1" || got.Text != string([]rune{0xff, 0xfe, 'A'}) {
		t.Fatalf("latin1 fallback: %+v", got)
	}
	if got := DecodeOutput([]byte("plain"), "shift_jis"); got.Text != "plain" || got.Encoding != "utf-8" {
		t.Fatalf("utf8 strict: %+v", got)
	}
}

func TestCmdArgsUtf8AndPlain(t *testing.T) {
	args, env := CmdArgs("echo a & echo b", true, "C:/Windows/System32/cmd.exe")
	if env["KLOUD_KODE_CMD"] != "echo a & echo b" {
		t.Fatalf("env: %v", env)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "chcp 65001>nul") || !strings.Contains(joined, "!KLOUD_KODE_CMD!") {
		t.Fatalf("utf8 args: %q", args)
	}
	plain, env := CmdArgs("echo hi", false, "cmd.exe")
	q := string([]byte{34})
	if len(env) != 0 || plain[len(plain)-1] != q+"echo hi"+q {
		t.Fatalf("plain args: %q env %v", plain, env)
	}
}

func TestPowerShellArgsEncodedAndLongFile(t *testing.T) {
	args, file := PowerShellArgs("Write-Output ok", true, false, false, "")
	if file != nil || len(args) < 2 || args[len(args)-2] != "-EncodedCommand" {
		t.Fatalf("encoded args: %q file %+v", args, file)
	}
	raw, err := base64.StdEncoding.DecodeString(args[len(args)-1])
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("encoded script: %v", err)
	}
	long := strings.Repeat("x", MaxEncodedCommand)
	args, file = PowerShellArgs(long, true, false, false, "C:/tmp")
	if file == nil || !strings.HasSuffix(file.Path, ".ps1") || !strings.HasPrefix(file.Content, string([]rune{0xFEFF})) {
		t.Fatalf("file args: %q %+v", args, file)
	}
	if got := PowerShellScript("Write-Output x", true); !strings.Contains(got, "LASTEXITCODE") || !strings.Contains(got, "TrimEnd(' ')") {
		t.Fatalf("script envelope: %s", got)
	}
}
func TestDetectShellsWindowsSimulated(t *testing.T) {
	sep := string([]byte{92})
	win := func(parts ...string) string { return strings.Join(parts, sep) }
	pwshDir := win("D:", "tools", "PowerShell")
	system32 := win("C:", "Windows", "System32")
	customPwsh := win("C:", "custom", "pwsh", "7")
	toolsBin := win("C:", "tools", "bin")
	env := map[string]string{
		"SystemRoot": win("C:", "Windows"),
		"PATH":       strings.Join([]string{pwshDir, system32, customPwsh, toolsBin}, ";"),
	}
	files := map[string]bool{
		win(pwshDir, "pwsh.exe"):    true,
		win(customPwsh, "pwsh.exe"): true,
		win(system32, "bash.exe"):   true,
		win(toolsBin, "bash.exe"):   true,
	}
	got := DetectShells("win32", env, func(p string) bool { return files[p] })
	if want := win(pwshDir, "pwsh.exe"); got["pwsh"] != want {
		t.Fatalf("pwsh: %q want %q", got["pwsh"], want)
	}
	if want := win(toolsBin, "bash.exe"); got["bash"] != want {
		t.Fatalf("bash: %q want %q", got["bash"], want)
	}
}
