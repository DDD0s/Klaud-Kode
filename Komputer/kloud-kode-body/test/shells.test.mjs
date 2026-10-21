// Shell selection and the PowerShell wrapper. Windows itself is simulated
// (detectShells takes the environment and an exists() probe); the wrapper's
// behaviour is checked against a real PowerShell 7 when one is installed:
// set KLOUD_KODE_TEST_PWSH to its directory, or put pwsh on PATH.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { cmdArgs, decodeOutput, detectShells, powershellArgs, powershellScript, runCommand, Processes } from "../server.mjs";

if (process.env.KLOUD_KODE_TEST_PWSH) process.env.PATH = `${process.env.KLOUD_KODE_TEST_PWSH}${path.delimiter}${process.env.PATH}`;
const pwsh = String(process.env.PATH).split(path.delimiter).some((d) => d && fs.existsSync(path.join(d, "pwsh")));

test("Windows shells: Git Bash is bash, WSL's System32 bash.exe is not, pwsh is found on PATH", () => {
  const files = new Set([
    "C:\\Windows\\System32\\cmd.exe",
    "C:\\Windows\\System32\\bash.exe",
    "C:\\Windows\\System32\\wsl.exe",
    "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
    "C:\\Program Files\\Git\\bin\\bash.exe",
    "C:\\Tools\\pwsh\\pwsh.exe",
  ]);
  const env = { SystemRoot: "C:\\Windows", ComSpec: "C:\\Windows\\System32\\cmd.exe", ProgramFiles: "C:\\Program Files",
    PATH: "C:\\Windows\\System32;C:\\Tools\\pwsh" };
  const s = detectShells({ platform: "win32", env, exists: (p) => files.has(p) });
  assert.equal(s.cmd, "C:\\Windows\\System32\\cmd.exe");
  assert.equal(s.pwsh, "C:\\Tools\\pwsh\\pwsh.exe");
  assert.equal(s.powershell, "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe");
  assert.equal(s.bash, "C:\\Program Files\\Git\\bin\\bash.exe");
  assert.equal(s.wsl, "C:\\Windows\\System32\\wsl.exe");
  assert.equal(s.sh, null);

  files.delete("C:\\Program Files\\Git\\bin\\bash.exe");
  const noGit = detectShells({ platform: "win32", env, exists: (p) => files.has(p) });
  assert.equal(noGit.bash, null, "WSL's launcher in System32 does not count as bash");
  files.add("C:\\Tools\\git\\bin\\bash.exe");
  const onPath = detectShells({ platform: "win32", env: { ...env, PATH: `${env.PATH};C:\\Tools\\git\\bin` }, exists: (p) => files.has(p) });
  assert.equal(onPath.bash, "C:\\Tools\\git\\bin\\bash.exe", "a bash.exe on PATH outside System32 is used");
  assert.equal(noGit.wsl, "C:\\Windows\\System32\\wsl.exe");
});

test("POSIX shells come from PATH", () => {
  const files = new Set(["/usr/bin/sh", "/usr/bin/bash"]);
  const s = detectShells({ platform: "linux", env: { PATH: "/usr/bin" }, exists: (p) => files.has(p) });
  assert.deepEqual([s.sh, s.bash, s.pwsh, s.cmd, s.wsl], ["/usr/bin/sh", "/usr/bin/bash", null, null, null]);
});

test("the PowerShell script and arguments: UTF-8, plain-text errors, bash exit status, long scripts via a file", () => {
  const script = powershellScript("Get-ChildItem");
  assert.match(script, /OutputEncoding = New-Object System\.Text\.UTF8Encoding \$false/);
  assert.match(script, /\n\. \{\nGet-ChildItem\n\$global:__komputerOk = \$\? \} \| Out-String -Stream -Width 4096\nif \(-not \$global:__komputerOk\)/);
  assert.doesNotMatch(powershellScript("x", { utf8: false }), /UTF8Encoding/);

  const short = powershellArgs("Get-Date");
  assert.deepEqual(short.args.slice(0, -1), ["-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-InputFormat", "None", "-OutputFormat", "Text", "-EncodedCommand"]);
  assert.equal(Buffer.from(short.args.at(-1), "base64").toString("utf16le"), powershellScript("Get-Date"));
  assert.equal(short.file, null);
  assert.ok(!powershellArgs("Get-Date", { interactive: true }).args.includes("-NonInteractive"), "started processes may prompt");

  const long = powershellArgs("Write-Output '" + "x".repeat(20_000) + "'", { tmpDir: "/tmp" });
  assert.equal(long.args.at(-2), "-File");
  assert.ok(long.file.content.startsWith("\uFEFF"), "Windows PowerShell 5.1 needs the BOM to read UTF-8");
  assert.ok(long.file.path.endsWith(".ps1"));
  const ps51 = powershellArgs("Get-Date", { file: true, tmpDir: "/tmp" });
  assert.equal(ps51.args.at(-2), "-File", "Windows PowerShell 5.1 ignores -OutputFormat Text with -EncodedCommand");
  assert.ok(ps51.file.content.endsWith(powershellScript("Get-Date")));
});

test("cmd: the command reaches a second cmd started after chcp 65001, through one parse", () => {
  const c = cmdArgs(`echo "a & b"! & exit /b 5`, { exe: "C:\\Windows\\System32\\cmd.exe" });
  assert.deepEqual(c.args, ["/d", "/v:on", "/s", "/c", `"chcp 65001>nul & "C:\\Windows\\System32\\cmd.exe" /d /s /c "!KLOUD_KODE_CMD!""`]);
  assert.deepEqual(c.env, { KLOUD_KODE_CMD: `echo "a & b"! & exit /b 5` });
  assert.deepEqual(cmdArgs("dir", { utf8: false }), { args: ["/d", "/s", "/c", `"dir"`], env: {} });
});

test("PowerShell (real pwsh): exit codes follow bash, output is UTF-8, errors are text, long scripts run", { skip: !pwsh && "pwsh is not installed" }, async () => {
  const run = (command) => runCommand({ command, shell: "pwsh", cwd: process.cwd(), timeoutMs: 30_000, maxOutputBytes: 1 << 20 });
  const cases = [
    ["sh -c 'exit 3'", 3],
    ["sh -c 'exit 3'; Write-Output ok", 0],
    ["Get-Item /nonexistent-komputer", 1],
    ["Write-Output a; exit 7", 7],
    ["throw 'boom'", 1],
  ];
  for (const [command, code] of cases) assert.equal((await run(command)).exitCode, code, command);
  const err = await run("Get-Item /nonexistent-komputer");
  assert.match(err.stderr, /Cannot find path/);
  assert.doesNotMatch(err.stderr, /CLIXML/);
  const utf = await run("Write-Output '中文 ünï'");
  assert.equal(utf.stdout.trim(), "中文 ünï");
  assert.equal(utf.encoding, "utf-8");
  const quoting = await run(`$x = "a 'b' \`"c\`""; Write-Output "[$x]"`);
  assert.equal(quoting.stdout.trim(), `[a 'b' "c"]`);
  const big = await run(`$s = '${"y".repeat(20_000)}'; Write-Output $s.Length`);
  assert.equal(big.stdout.trim(), "20000", big.stderr);
  const native = await run("sh -c 'printf \"\\344\\270\\255 native\\n\"'; Write-Output $LASTEXITCODE");
  assert.equal(native.stdout, "中 native\n0\n");
  const scoped = await run("$n = 2; function Twice($x) { $x * $n }; Twice 21");
  assert.equal(scoped.stdout.trim(), "42");
  const name = "a-file-name-long-enough-to-wrap-in-an-eighty-column-table-on-the-users-computer.txt";
  const dir = fs.mkdtempSync(path.join(process.cwd(), ".pwsh-wide-"));
  try {
    fs.writeFileSync(path.join(dir, name), "");
    const table = await runCommand({ command: "Get-ChildItem -Force", shell: "pwsh", cwd: dir, timeoutMs: 30_000, maxOutputBytes: 1 << 20 });
    assert.ok(table.stdout.split("\n").some((l) => l.endsWith(name)), `tables are not wrapped at 80 columns:\n${table.stdout}`);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("output that is not UTF-8 is decoded with the console code page, e.g. GBK", () => {
  const gbk = Buffer.from([0xd6, 0xd0, 0xce, 0xc4, 0x0d, 0x0a]);
  assert.deepEqual(decodeOutput(gbk, "gbk"), { text: "中文\r\n", encoding: "gbk" });
  assert.deepEqual(decodeOutput(Buffer.from("中文\r\n"), "gbk"), { text: "中文\r\n", encoding: "utf-8" }, "UTF-8 wins when it is valid");
});

test("a started process with idleTimeoutMs is stopped once nobody reads it", async () => {
  const p = new Processes(1 << 16);
  const { processId } = p.start("sleep 30", process.cwd(), "default", { idleTimeoutMs: 1_000 });
  await new Promise((r) => setTimeout(r, 600));
  assert.equal(p.read(processId).running, true, "a read renews the lease");
  await new Promise((r) => setTimeout(r, 2_600));
  const r = p.read(processId);
  assert.equal(r.running, false);
  assert.equal(r.idleStopped, true);
  const kept = p.start("sleep 30", process.cwd());
  await new Promise((r) => setTimeout(r, 1_200));
  assert.equal(p.read(kept.processId).running, true, "without a lease the process keeps running");
  p.killAll();
});
