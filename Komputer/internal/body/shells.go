package body

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
)

// ShellNames are the shell values run_command accepts.
var ShellNames = []string{"default", "cmd", "powershell", "pwsh", "bash", "sh", "wsl"}

// IsWindows reports whether this process runs on Windows.
var IsWindows = runtime.GOOS == "windows"

// DefaultShellName is what "default" means on this machine, in words the
// model can act on.
func DefaultShellName() string {
	if IsWindows {
		return "cmd.exe"
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return filepath.Base(shell)
}

// CommandEnv is the environment for spawned commands: the full environment
// (stripping it breaks ordinary tooling), minus this server's own secrets.
func CommandEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for k := range env {
		upper := strings.ToUpper(k)
		if upper == "KLAUD_KODE_BODY_TOKEN" || upper == "KLAUD_KODE_BODY_CONFIG" {
			delete(env, k)
		}
	}
	return env
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

type pathOps struct {
	delimiter string
	join      func(elem ...string) string
	dirname   func(string) string
}

func posixOps() pathOps {
	return pathOps{delimiter: ":", join: pathpkg.Join, dirname: pathpkg.Dir}
}

func winJoin(elem ...string) string {
	parts := []string{}
	for _, e := range elem {
		if e != "" {
			parts = append(parts, e)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	joined := parts[0]
	for _, p := range parts[1:] {
		joined = strings.TrimRight(joined, string([]byte{92, '/'})) + string([]byte{92}) + strings.TrimLeft(p, string([]byte{92, '/'}))
	}
	return joined
}

func winDirname(p string) string {
	p = strings.TrimRight(p, string([]byte{92, '/'}))
	i := strings.LastIndexAny(p, string([]byte{92, '/'}))
	if i < 0 {
		return "."
	}
	if i == 0 {
		return p[:1]
	}
	return p[:i]
}

func winOps() pathOps { return pathOps{delimiter: ";", join: winJoin, dirname: winDirname} }

func onPath(names []string, env map[string]string, exists func(string) bool, ops pathOps, skip func(string) bool) string {
	pathValue := env["PATH"]
	if pathValue == "" {
		pathValue = env["Path"]
	}
	for _, dir := range strings.Split(pathValue, ops.delimiter) {
		if dir == "" {
			continue
		}
		for _, name := range names {
			p := ops.join(dir, name)
			if (skip == nil || !skip(p)) && exists(p) {
				return p
			}
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// DetectShells returns installed shell paths by accepted name ("" missing).
// On Windows "bash" means Git Bash; System32's bash.exe is WSL's launcher
// and is reported as "wsl" instead. The platform parameter uses Node-style
// names ("win32" for Windows) so unit tests can simulate either host.
func DetectShells(platform string, env map[string]string, exists func(string) bool) map[string]string {
	if platform == "" {
		if runtime.GOOS == "windows" {
			platform = "win32"
		} else {
			platform = runtime.GOOS
		}
	}
	if env == nil {
		env = CommandEnv()
	}
	if exists == nil {
		exists = fileExists
	}
	if platform != "win32" {
		ops := posixOps()
		posix := func(names []string) string { return onPath(names, env, exists, ops, nil) }
		sh := posix([]string{"sh"})
		if sh == "" && exists("/bin/sh") {
			sh = "/bin/sh"
		}
		return map[string]string{
			"default": sh, "sh": sh, "bash": posix([]string{"bash"}),
			"pwsh": posix([]string{"pwsh"}), "powershell": "", "cmd": "", "wsl": "",
		}
	}
	ops := winOps()
	sys := env["SystemRoot"]
	if sys == "" {
		sys = env["SYSTEMROOT"]
	}
	if sys == "" {
		sys = "C:" + string([]byte{92}) + "Windows"
	}
	first := func(candidates ...string) string {
		for _, c := range candidates {
			if c != "" && exists(c) {
				return c
			}
		}
		return ""
	}
	system32 := strings.ToLower(ops.join(sys, "System32"))
	win := func(names []string, skip func(string) bool) string { return onPath(names, env, exists, ops, skip) }
	comSpec := env["ComSpec"]
	if comSpec == "" {
		comSpec = env["COMSPEC"]
	}
	cmd := first(comSpec, ops.join(sys, "System32", "cmd.exe"))
	programFiles := env["ProgramFiles"]
	programFilesX86 := env["ProgramFiles(x86)"]
	localAppData := env["LOCALAPPDATA"]
	joinIf := func(base string, elem ...string) string {
		if base == "" {
			return ""
		}
		return ops.join(append([]string{base}, elem...)...)
	}
	powershell := first(ops.join(sys, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"))
	if powershell == "" {
		powershell = win([]string{"powershell.exe"}, nil)
	}
	pwsh := win([]string{"pwsh.exe"}, nil)
	if pwsh == "" {
		pwsh = first(joinIf(programFiles, "PowerShell", "7", "pwsh.exe"))
	}
	bash := first(
		joinIf(programFiles, "Git", "bin", "bash.exe"),
		joinIf(programFilesX86, "Git", "bin", "bash.exe"),
		joinIf(localAppData, "Programs", "Git", "bin", "bash.exe"),
	)
	if bash == "" {
		bash = win([]string{"bash.exe"}, func(c string) bool {
			return strings.EqualFold(ops.dirname(c), system32)
		})
	}
	return map[string]string{
		"default": cmd, "cmd": cmd, "powershell": powershell, "pwsh": pwsh,
		"bash": bash, "sh": "", "wsl": first(ops.join(sys, "System32", "wsl.exe")),
	}
}

var detectedShells struct {
	sync.Once
	values map[string]string
}

// ShellPath returns the detected path for one shell on this host.
func ShellPath(name string) string {
	detectedShells.Do(func() {
		platform := runtime.GOOS
		if platform == "windows" {
			platform = "win32"
		}
		detectedShells.values = DetectShells(platform, nil, nil)
	})
	return detectedShells.values[name]
}

// PowerShellScript renders the wrapper script a command runs inside: UTF-8
// console output, no progress records, and bash's exit-status rule.
func PowerShellScript(command string, utf8 bool) string {
	lines := []string{"$ProgressPreference = 'SilentlyContinue'"}
	if utf8 {
		lines = append(lines,
			"try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false } catch {}",
			"try { [Console]::InputEncoding = New-Object System.Text.UTF8Encoding $false } catch {}",
			"$OutputEncoding = New-Object System.Text.UTF8Encoding $false",
		)
	}
	lines = append(lines,
		"$global:LASTEXITCODE = 0",
		"$global:__komputerOk = $true",
		". {",
		command,
		"$global:__komputerOk = $? } | Out-String -Stream -Width 4096 | ForEach-Object { $_.TrimEnd(' ') }",
		"if (-not $global:__komputerOk) { if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1 }",
		"exit 0",
	)
	return strings.Join(lines, string([]byte{10}))
}

// MaxEncodedCommand bounds -EncodedCommand scripts; longer ones use a file.
const MaxEncodedCommand = 30_000

// PowerShellFile is a temporary script the caller must write and remove.
type PowerShellFile struct {
	Path    string
	Content string
}

// PowerShellArgs renders powershell/pwsh arguments for one command. With
// -EncodedCommand or -File every quoting layer is sidestepped. Windows
// PowerShell 5.1 does not honour -OutputFormat Text for encoded commands,
// so it always runs through a temporary file.
func PowerShellArgs(command string, utf8, interactive, viaFile bool, tmpDir string) (args []string, file *PowerShellFile) {
	script := PowerShellScript(command, utf8)
	head := []string{"-NoLogo", "-NoProfile"}
	if !interactive {
		head = append(head, "-NonInteractive")
	}
	head = append(head, "-ExecutionPolicy", "Bypass", "-InputFormat", "None", "-OutputFormat", "Text")
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 0, len(units)*2)
	for _, u := range units {
		raw = append(raw, byte(u), byte(u>>8))
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if !viaFile && len(encoded) <= MaxEncodedCommand {
		return append(head, "-EncodedCommand", encoded), nil
	}
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	path := filepath.Join(tmpDir, "klaud-kode-"+randomID()+".ps1")
	// Windows PowerShell 5.1 reads a BOM-less script as ANSI.
	return append(head, "-File", path), &PowerShellFile{Path: path, Content: string([]rune{0xFEFF}) + script}
}

// CmdArgs renders cmd.exe arguments. With UTF-8, a first cmd switches the
// code page and a second cmd runs the command, which travels in an
// environment variable read with delayed expansion: the outer cmd never
// parses it and the inner one parses it exactly once.
func CmdArgs(command string, utf8 bool, exe string) (args []string, env map[string]string) {
	if exe == "" {
		exe = "cmd.exe"
	}
	q := string([]byte{34})
	if !utf8 {
		return []string{"/d", "/s", "/c", q + command + q}, map[string]string{}
	}
	inner := q + "chcp 65001>nul & " + q + exe + q + " /d /s /c " + q + "!KLAUD_KODE_CMD!" + q + q
	return []string{"/d", "/v:on", "/s", "/c", inner}, map[string]string{"KLAUD_KODE_CMD": command}
}
func randomID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// spawnResult is one constructed (not yet started) shell process.
type spawnResult struct {
	cmd     *exec.Cmd
	cleanup func()
}

// SpawnShell constructs a command in the requested shell. "default" is the
// platform shell (cmd.exe on Windows, sh elsewhere).
func SpawnShell(command, shell, dir string, utf8, interactive bool) (*exec.Cmd, func(), error) {
	res, err := buildShell(command, shell, dir, utf8, interactive)
	if err != nil {
		return nil, nil, err
	}
	return res.cmd, res.cleanup, nil
}

func buildShell(command, shell, dir string, utf8, interactive bool) (spawnResult, error) {
	env := CommandEnv()
	newCmd := func(exe string, args []string) *exec.Cmd {
		cmd := exec.Command(exe, args...)
		cmd.Dir = dir
		cmd.Env = envList(env)
		setProcessGroup(cmd)
		return cmd
	}
	if IsWindows && shell == "default" {
		shell = "cmd"
	}
	if !IsWindows && shell == "default" {
		shell = "sh"
	}
	switch shell {
	case "powershell", "pwsh":
		exe := ShellPath(shell)
		if exe == "" {
			if shell == "pwsh" {
				exe = "pwsh"
			} else {
				exe = "powershell.exe"
			}
		}
		args, file := PowerShellArgs(command, utf8, interactive, shell == "powershell", "")
		cleanup := func() {}
		if file != nil {
			if err := os.WriteFile(file.Path, []byte(file.Content), 0o600); err != nil {
				return spawnResult{}, err
			}
			cleanup = func() { _ = os.Remove(file.Path) }
		}
		return spawnResult{cmd: newCmd(exe, args), cleanup: cleanup}, nil
	case "cmd":
		exe := ShellPath("cmd")
		if exe == "" {
			exe = os.Getenv("COMSPEC")
		}
		if exe == "" {
			exe = "cmd.exe"
		}
		args, extra := CmdArgs(command, utf8 && IsWindows, exe)
		for k, v := range extra {
			env[k] = v
		}
		cmd := newCmd(exe, args)
		cmd.Env = envList(env)
		if IsWindows {
			setCmdLine(cmd, quoteWindowsExe(exe)+" "+strings.Join(args, " "))
		}
		return spawnResult{cmd: cmd, cleanup: func() {}}, nil
	case "wsl":
		exe := ShellPath("wsl")
		if exe == "" {
			return spawnResult{}, fmt.Errorf("WSL is not installed on this computer")
		}
		return spawnResult{cmd: newCmd(exe, []string{"-e", "bash", "-c", command}), cleanup: func() {}}, nil
	case "bash", "sh":
		if IsWindows && shell == "bash" && ShellPath("bash") == "" {
			return spawnResult{}, fmt.Errorf("bash is not installed on this computer (Git for Windows provides it)")
		}
		exe := shell
		if IsWindows {
			if p := ShellPath(shell); p != "" {
				exe = p
			}
		}
		return spawnResult{cmd: newCmd(exe, []string{"-c", command}), cleanup: func() {}}, nil
	default:
		// An unrecognized shell runs through the platform shell, as before.
		if IsWindows {
			return buildShell(command, "cmd", dir, utf8, interactive)
		}
		return spawnResult{cmd: newCmd("/bin/sh", []string{"-c", command}), cleanup: func() {}}, nil
	}
}

func quoteWindowsExe(exe string) string {
	if strings.ContainsAny(exe, " \t") {
		q := string([]byte{34})
		return q + exe + q
	}
	return exe
}
