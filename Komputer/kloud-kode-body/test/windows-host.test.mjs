// The shells on a real Windows host (CI runs this on windows-latest): Windows
// PowerShell 5.1, pwsh, cmd with code page 65001, Git Bash, and an interactive
// process fed through stdin. Skipped elsewhere.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { detectShells, runCommand, Processes } from "../server.mjs";

const skip = process.platform !== "win32" && "not a Windows host";
const shells = process.platform === "win32" ? detectShells() : {};
const run = (command, shell, cwd = process.cwd()) => runCommand({ command, shell, cwd, timeoutMs: 60_000, maxOutputBytes: 1 << 20 });
const until = async (check, ms = 20_000) => {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 200))) if (check()) return true;
  return false;
};

test("Windows: the shells are found", { skip }, () => {
  assert.match(shells.cmd, /cmd\.exe$/i);
  assert.match(shells.powershell, /WindowsPowerShell\\v1\.0\\powershell\.exe$/i);
  assert.ok(shells.pwsh, "GitHub's Windows runners have PowerShell 7");
  assert.ok(shells.bash && !/System32/i.test(shells.bash), `Git Bash, not WSL's launcher: ${shells.bash}`);
});

for (const shell of ["powershell", "pwsh"]) {
  test(`Windows: ${shell} exit codes, UTF-8, plain-text errors, Windows paths, wide tables`, { skip }, async () => {
    assert.equal((await run("cmd /c exit 3", shell)).exitCode, 3);
    assert.equal((await run("cmd /c exit 3; Write-Output ok", shell)).exitCode, 0);
    assert.equal((await run("Write-Output a; exit 7", shell)).exitCode, 7);
    assert.equal((await run("throw 'boom'", shell)).exitCode, 1);
    const err = await run("Get-Item C:\\nonexistent-komputer", shell);
    assert.equal(err.exitCode, 1);
    assert.match(err.stderr, /Cannot find path/);
    assert.doesNotMatch(err.stderr, /CLIXML/);

    const utf = await run("Write-Output '中文 ünï'", shell);
    assert.equal(utf.stdout.trim(), "中文 ünï");
    assert.equal(utf.encoding, "utf-8");
    const native = await run("cmd /c echo 中文 native", shell);
    assert.equal(native.stdout.trim(), "中文 native", "a native child's output survives the round trip");

    const dir = fs.mkdtempSync(path.join(process.cwd(), ".kk-win-"));
    try {
      const name = "a-file-name-long-enough-to-wrap-in-an-eighty-column-table-on-the-users-computer.txt";
      fs.writeFileSync(path.join(dir, name), "x\r\n");
      const where = await run("(Get-Location).Path", shell, dir);
      assert.equal(where.stdout.trim().toLowerCase(), dir.toLowerCase());
      const table = await run("Get-ChildItem -Force", shell, dir);
      assert.ok(table.stdout.split(/\r?\n/).some((l) => l.endsWith(name)), table.stdout);
      assert.match(table.stdout, /\r\n/, "Windows output keeps CRLF");
    } finally { fs.rmSync(dir, { recursive: true, force: true }); }
  });
}

test("Windows: cmd under code page 65001, Git Bash", { skip }, async () => {
  const cmd = await run("echo 中文 & exit /b 5", "cmd");
  assert.equal(cmd.exitCode, 5);
  assert.equal(cmd.stdout.trim(), "中文");
  assert.equal(cmd.encoding, "utf-8");
  const bash = await run("echo \"$((1+2)) $(uname -o)\"; exit 4", "bash");
  assert.equal(bash.exitCode, 4);
  assert.match(bash.stdout, /^3 Msys/);
});

test("Windows: an interactive PowerShell process reads stdin; stop kills its tree", { skip }, async () => {
  const p = new Processes(1 << 16);
  try {
    const { processId } = p.start("$n = Read-Host 'name'; Write-Output \"hi $n\"; cmd /c ping -n 30 127.0.0.1", process.cwd(), "powershell");
    await until(() => /name/.test(p.read(processId).stdout));
    p.write(processId, "bob\r\n");
    assert.ok(await until(() => /hi bob/.test(p.read(processId).stdout)), p.read(processId).stdout + p.read(processId).stderr);
    assert.equal(p.read(processId).running, true);
    p.stop(processId);
    assert.ok(await until(() => !p.read(processId).running), "taskkill /t ends PowerShell and its ping");
  } finally { p.killAll(); }
});
