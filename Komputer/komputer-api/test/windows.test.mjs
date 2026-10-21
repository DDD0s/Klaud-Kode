// agy on a Linux host driving a Windows computer: path mapping, line endings,
// the shell the command runs in, the POSIX fallback and the model's environment note.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { commandToWindows, crlfToLf, environmentNote, isWindowsPath, looksPosix, mountToWindows, translatePosix, windowsMountDir, windowsToMount }
  from "../backends/windows.mjs";
import { decide, lineEndingFixes } from "../backends/agy-hook.mjs";
import { plan } from "../backends/agy-bash-shim.mjs";

const root = "C:\\Users\\me\\proj";
const mount = "/srv/k/remote/c/Users/me/proj";
const r = { root, mount };

test("Windows paths: the mount mirrors C:\\ as <base>/c, case-insensitively, and nothing outside the root maps", () => {
  assert.ok(isWindowsPath("C:\\x") && isWindowsPath("c:/x") && isWindowsPath("D:") && isWindowsPath("\\\\nas\\share"));
  assert.ok(!isWindowsPath("/c/x") && !isWindowsPath("src\\a") && !isWindowsPath("C:x"));
  assert.equal(windowsMountDir("/srv/k/remote", root), mount);
  assert.equal(windowsMountDir("/mnt", "D:\\"), "/mnt/d", "with base /mnt the paths read like WSL's");
  assert.equal(windowsMountDir("/mnt", "\\\\nas\\share\\x"), "/mnt/unc/nas/share/x");
  assert.equal(windowsToMount("c:/users/ME/proj/Src\\a.txt", r), `${mount}/Src/a.txt`);
  assert.equal(windowsToMount("C:\\Users\\me\\proj", r), mount);
  assert.equal(windowsToMount("C:\\Users\\me\\proj\\", r), mount);
  assert.equal(windowsToMount("C:\\Users\\me\\projX\\a", r), null, "a sibling with the same prefix is outside");
  assert.equal(windowsToMount("C:\\Users\\me\\proj\\..\\secret", r), null);
  assert.equal(windowsToMount("D:\\proj\\a", r), null);
  assert.equal(mountToWindows(`${mount}/sub/x.md`, r), "C:\\Users\\me\\proj\\sub\\x.md");
  assert.equal(mountToWindows(mount, r), root);
  assert.equal(mountToWindows(`${mount}/sub/x.md`, r, "/"), "C:/Users/me/proj/sub/x.md");
  assert.equal(mountToWindows("/etc/hosts", r), null);
  assert.equal(windowsToMount("D:\\a", { root: "D:\\", mount: "/mnt/d" }), "/mnt/d/a", "a drive root");
});

test("commands: mount paths become Windows paths; CRLF output becomes LF across chunks", () => {
  assert.equal(commandToWindows(`Get-Content ${mount}/sub/x.md; cd ${mount}`, r), "Get-Content C:\\Users\\me\\proj\\sub\\x.md; cd C:\\Users\\me\\proj");
  assert.equal(commandToWindows(`cat "${mount}/a b.txt"`, r), 'cat "C:\\Users\\me\\proj\\a b.txt"');
  assert.equal(commandToWindows(`ls ${mount}x`, r), `ls ${mount}x`, "only the mount itself, not a longer name");
  assert.equal(commandToWindows(`cat ${mount}/a.txt`, r, "/"), "cat C:/Users/me/proj/a.txt", "Git Bash takes forward slashes");
  const s = crlfToLf();
  assert.equal(["a\r", "\nb\r\n", "c\r"].map((c) => s.push(c)).join("") + s.end(), "a\nb\nc\r");
});

test("POSIX detection and translation to PowerShell", () => {
  for (const c of ["ls -la", "rm -rf build", "mkdir -p a/b", "grep -rn TODO .", "cat x 2>/dev/null", "export A=1", "cat <<EOF > x\nhi\nEOF", "if [ -f x ]; then echo y; fi", "head -n 3 a"]) {
    assert.ok(looksPosix(c), c);
  }
  for (const c of ["Get-ChildItem -Force", "ls", "dir", "cat a.txt", "echo hi", "Remove-Item -Recurse x; ls -la", "$env:PATH", "git status", "npm test", "python -c \"print(1)\""]) {
    assert.ok(!looksPosix(c), c);
  }
  const cases = {
    "ls -la": "Get-ChildItem -Force",
    "ls -la src": "Get-ChildItem -Force -LiteralPath 'src'",
    "ls *.txt": "Get-ChildItem -Path '*.txt'",
    "mkdir -p a/b && cd a": null,
    "mkdir -p a/b && touch a/b/c": "New-Item -ItemType Directory -Force -Path 'a/b' | Out-Null; if ($?) { if (Test-Path -LiteralPath 'a/b/c') { (Get-Item -LiteralPath 'a/b/c').LastWriteTime = Get-Date } else { New-Item -ItemType File -Path 'a/b/c' | Out-Null } }",
    "rm -rf build 2>/dev/null": "Remove-Item -LiteralPath 'build' -Recurse -Force -ErrorAction SilentlyContinue 2>$null",
    "cat a.txt b.txt": "Get-Content -LiteralPath 'a.txt','b.txt'",
    "head -n 5 x.log": "Get-Content -LiteralPath 'x.log' -TotalCount 5",
    "tail -20 x.log": "Get-Content -LiteralPath 'x.log' -Tail 20",
    "grep -rn TODO src": "Get-ChildItem -LiteralPath 'src' -Recurse -File | Select-String -Pattern 'TODO' -CaseSensitive",
    "grep -i 'it''s' a.txt": "Select-String -Pattern 'its' -LiteralPath 'a.txt'",
    'grep "a$b" x': null,
    "grep -il todo a.txt": "Select-String -Pattern 'todo' -List -LiteralPath 'a.txt' | ForEach-Object Path",
    "export FOO=bar && echo ok": "$env:FOO = 'bar'; if ($?) { Write-Output 'ok' }",
    "which node": "(Get-Command 'node' -ErrorAction Stop).Source",
    "uname -a": "[System.Environment]::OSVersion.VersionString",
    "wc -l a.txt": "(Get-Content -LiteralPath 'a.txt' | Measure-Object -Line).Lines",
    "cp -r a b": "Copy-Item -LiteralPath 'a' -Destination 'b' -Recurse -Force",
    "mv 'my file' dest": "Move-Item -LiteralPath 'my file' -Destination 'dest' -Force",
    "ls -la | grep x": null,
    "echo $HOME": null,
    "find . -name '*.js'": null,
    "ls -lt": null,
  };
  for (const [posix, ps] of Object.entries(cases)) assert.equal(translatePosix(posix), ps, posix);
});

test("shim plan: PowerShell by default, POSIX slips go to Git Bash or a translation, other platforms unchanged", () => {
  const remote = { ...r, platform: "win32", shell: "powershell", posixFallback: "auto", bash: false };
  assert.deepEqual(plan(`Get-Content ${mount}/a.txt`, remote), { shell: "powershell", text: "Get-Content C:\\Users\\me\\proj\\a.txt", note: "" });
  const translated = plan("ls -la", remote);
  assert.equal(translated.text, "Get-ChildItem -Force");
  assert.match(translated.note, /This computer runs Windows; ran as PowerShell: Get-ChildItem -Force/);
  const bash = plan(`ls -la ${mount}/src | grep x`, { ...remote, bash: true });
  assert.deepEqual([bash.shell, bash.text], ["bash", "ls -la C:/Users/me/proj/src | grep x"]);
  assert.match(bash.note, /ran in Git Bash/);
  assert.equal(plan("ls -la", { ...remote, bash: true, posixFallback: "translate" }).text, "Get-ChildItem -Force");
  assert.deepEqual(plan("ls -la | grep x", { ...remote, posixFallback: "bash" }), { shell: "powershell", text: "ls -la | grep x", note: "" }, "no Git Bash: run as written");
  assert.equal(plan("ls -la", { ...remote, posixFallback: "off" }).text, "ls -la");
  assert.equal(plan("ls -la", { ...remote, shell: "cmd" }).shell, "cmd", "an explicit cmd shell is not second-guessed");
  assert.deepEqual(plan("ls -la /ws/remote", { mount: "/ws/remote", root: "/Users/me/p", platform: "darwin" }), { shell: "bash", text: "ls -la /Users/me/p", note: "" });
});

test("hook on a Windows computer: C:\\ paths move onto the mount, others are denied with the Windows root named", () => {
  const policy = { mode: "remote", mount, root, platform: "win32", files: true, commands: true };
  const call = (name, args) => decide({ toolCall: { name, args }, conversationId: "c1" }, policy, { home: "/home/svc" });
  assert.deepEqual(call("view_file", { AbsolutePath: "C:\\Users\\me\\proj\\src\\app.js" }), { decision: "allow", overwrite: { AbsolutePath: `${mount}/src/app.js` } });
  assert.deepEqual(call("list_dir", { DirectoryPath: "c:/users/me/proj" }), { decision: "allow", overwrite: { DirectoryPath: mount } });
  assert.deepEqual(call("read_file", { Path: "src\\app.js" }), { decision: "allow", overwrite: { Path: `${mount}/src/app.js` } }, "a relative Windows path");
  assert.equal(call("read_file", { Path: `${mount}/a.txt` }), null);
  assert.deepEqual(call("run_command", { CommandLine: "dir", Cwd: "C:\\Users\\me\\proj\\src" }), { decision: "allow", overwrite: { Cwd: `${mount}/src` } });
  assert.match(call("read_file", { Path: "D:\\other\\x.txt" }).reason, /^D:\\other\\x\.txt is outside the user's workspace C:\\Users\\me\\proj; read_file can only use paths inside it\. run_command reaches/);
  assert.match(call("read_file", { Path: "C:\\Windows\\win.ini" }).reason, /outside the user's workspace C:\\Users\\me\\proj/);
  assert.match(call("read_file", { Path: "/etc/passwd" }).reason, /service host/);
  const commandsOnly = { ...policy, files: false, mount: "/ws/body" };
  assert.match(decide({ toolCall: { name: "read_file", args: { Path: "C:\\Users\\me\\proj\\a.txt" } } }, commandsOnly).reason, /Get-Content, Get-ChildItem or Set-Content/);
});

test("hook line endings: LF edits of CRLF files become CRLF, whole-file writes keep a CRLF file CRLF", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "agy-eol-"));
  const crlf = path.join(dir, "crlf.txt");
  const lf = path.join(dir, "lf.txt");
  const mixed = path.join(dir, "mixed.txt");
  fs.writeFileSync(crlf, "line one\r\nline two\r\nline three\r\n");
  fs.writeFileSync(lf, "line one\nline two\n");
  fs.writeFileSync(mixed, "a\r\nb\nline one\r\nline two\r\n");
  const edit = { Path: crlf, Target: "line one\nline two", Replacement: "LINE ONE\nLINE TWO\n" };
  assert.deepEqual(lineEndingFixes("edit_file", edit, crlf), { Target: "line one\r\nline two", Replacement: "LINE ONE\r\nLINE TWO\r\n" });
  assert.deepEqual(lineEndingFixes("edit_file", { ...edit, Target: "line one" }, crlf), {}, "a single-line Target already matches");
  assert.deepEqual(lineEndingFixes("edit_file", edit, lf), {}, "LF files are left alone");
  assert.deepEqual(lineEndingFixes("edit_file", { ...edit, Target: "nowhere\nelse" }, crlf), {}, "a Target that matches neither way is left for agy to report");
  assert.deepEqual(lineEndingFixes("replace_file_content", { TargetFile: crlf, TargetContent: "line two\nline three", ReplacementContent: "x\ny" }, crlf),
    { TargetContent: "line two\r\nline three", ReplacementContent: "x\r\ny" });
  assert.deepEqual(lineEndingFixes("multi_replace_file_content", { TargetFile: crlf, ReplacementChunks: [{ TargetContent: "line one\nline two", ReplacementContent: "z" }, { TargetContent: "three", ReplacementContent: "3" }] }, crlf),
    { ReplacementChunks: [{ TargetContent: "line one\r\nline two", ReplacementContent: "z" }, { TargetContent: "three", ReplacementContent: "3" }] });
  assert.deepEqual(lineEndingFixes("write_file", { Path: crlf, Content: "new\ncontent\n", Overwrite: true }, crlf), { Content: "new\r\ncontent\r\n" });
  assert.deepEqual(lineEndingFixes("write_file", { Path: mixed, Content: "new\n" }, mixed), {}, "mixed files keep what the model wrote");
  assert.deepEqual(lineEndingFixes("write_file", { Path: path.join(dir, "new.txt"), Content: "a\n" }, path.join(dir, "new.txt")), {}, "new files keep what the model wrote");
  assert.deepEqual(lineEndingFixes("edit_file", { Path: mixed, Target: "line one\nline two", Replacement: "q" }, mixed), { Target: "line one\r\nline two" });

  const policy = { mode: "remote", mount: dir, root: "C:\\p", platform: "win32", files: true, commands: true };
  assert.deepEqual(decide({ toolCall: { name: "edit_file", args: { Path: "C:\\p\\crlf.txt", Target: "line one\nline two", Replacement: "1\n2" } } }, policy),
    { decision: "allow", overwrite: { Path: crlf, Target: "line one\r\nline two", Replacement: "1\r\n2" } }, "path and line endings in one overwrite");
  fs.rmSync(dir, { recursive: true, force: true });
});

test("environment note: Windows, its shell, the workspace both ways, and what the file tools reach", () => {
  const note = environmentNote({ platform: "win32", osVersion: "Windows 11 Pro", release: "10.0.26100", arch: "x64", root, mount, shell: "powershell", files: true, posixFallback: "auto", bash: false });
  assert.match(note, /^<user_computer>\n/);
  assert.match(note, /"The USER's OS version is linux", run_command's "Operating System: linux\. Shell: bash"/);
  assert.match(note, /runs Windows \(Windows 11 Pro 10\.0\.26100, x64\)/);
  assert.ok(note.includes(`- Workspace: ${root}. ${mount} is the same folder`));
  assert.match(note, /Windows PowerShell 5\.1 \(powershell\.exe\) in C:\\Users\\me\\proj\. Write PowerShell, not bash/);
  assert.match(note, /has no && or \|\|/);
  assert.match(note, /file tools reach only files inside C:\\Users\\me\\proj/);
  assert.match(note, /translated to PowerShell when it is simple/);
  const pwsh = environmentNote({ platform: "win32", root, mount, shell: "pwsh", files: false, posixFallback: "auto", bash: true });
  assert.match(pwsh, /PowerShell 7 \(pwsh\.exe\)/);
  assert.doesNotMatch(pwsh, /has no &&/);
  assert.match(pwsh, /cannot reach the user's files here: read and write them with run_command/);
  assert.match(pwsh, /is run in Git Bash/);
  assert.match(environmentNote({ platform: "darwin", osVersion: "Darwin Kernel" }), /runs macOS/);
  assert.equal(environmentNote({ platform: "linux" }), "");
  assert.equal(environmentNote({ platform: null }), "");
});
