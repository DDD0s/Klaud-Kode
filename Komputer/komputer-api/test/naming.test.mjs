import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const project = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
// Keep the retired marker out of the delivered source, including this guard.
const forbidden = new RegExp([
  [97, 103, 101, 110, 116, 118, 114],
  [99, 108, 111, 117, 100, 99, 111, 100, 101],
].map((codes) => String.fromCharCode(...codes)).join("|"), "i");
const bash = process.platform === "win32"
  ? path.join(process.env.ProgramFiles || "C:\\Program Files", "Git", "bin", "bash.exe")
  : "bash";

function sourceFiles() {
  return [...new Set(execFileSync("git", ["ls-files", "--cached", "--others", "--exclude-standard", "-z"], {
    cwd: project, encoding: "utf8",
  }).split("\0").filter(Boolean))].filter((rel) => fs.existsSync(path.join(project, rel)));
}

test("all current project source paths and contents exclude the retired internal name", () => {
  const files = sourceFiles();
  assert.ok(files.includes("komputer-api/server.mjs"));
  assert.ok(files.includes("komputer-session/.mcp.json"));
  assert.ok(files.includes("klaud-kode-body/server.mjs"));
  const manifest = JSON.parse(fs.readFileSync(path.join(project, "klaud-kode-body/package.json"), "utf8"));
  const lock = JSON.parse(fs.readFileSync(path.join(project, "klaud-kode-body/package-lock.json"), "utf8"));
  assert.equal(manifest.name, "klaud-kode-body");
  assert.deepEqual(manifest.bin, { "klaud-kode-body": "server.mjs" });
  assert.equal(lock.version, manifest.version);
  assert.equal(lock.packages[""].version, manifest.version);
  assert.deepEqual(lock.packages[""].bin, manifest.bin);
  for (const rel of files) {
    assert.doesNotMatch(rel, forbidden, `retired name in path: ${rel}`);
    const abs = path.join(project, rel);
    assert.equal(fs.lstatSync(abs).isFile(), true, `unexpected link or non-file in source: ${rel}`);
    assert.doesNotMatch(fs.readFileSync(abs, "utf8"), forbidden, `retired name in source: ${rel}`);
  }
});

test("renamed startup, tunnel and deployment scripts parse in bash", (t) => {
  if (spawnSync(bash, ["--version"]).error) return t.skip("bash is not installed");
  const scripts = sourceFiles().filter((rel) => rel.endsWith(".sh"));
  assert.ok(scripts.length >= 6);
  for (const rel of scripts) {
    const r = spawnSync(bash, ["-n", path.join(project, rel).replaceAll("\\", "/")], { encoding: "utf8" });
    assert.equal(r.status, 0, `${rel}: ${r.stderr}`);
  }
});

function deploymentFixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "komputer-deploy-test-"));
  const checkout = path.join(dir, "checkout");
  const app = path.join(checkout, "Komputer");
  const api = path.join(app, "komputer-api");
  fs.mkdirSync(checkout);
  const git = (...args) => execFileSync("git", [
    "-c", "commit.gpgsign=false", "-c", "user.name=Komputer Test", "-c", "user.email=test@example.invalid", ...args,
  ], { cwd: checkout, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  git("init", "--initial-branch=main");
  fs.mkdirSync(path.join(dir, "hooks"));
  git("config", "core.hooksPath", path.join(dir, "hooks").replaceAll("\\", "/"));
  fs.writeFileSync(path.join(checkout, "seed.txt"), "fixture");
  git("add", "seed.txt");
  git("commit", "-m", "fixture before layout");
  const previous = git("rev-parse", "HEAD");
  const origin = path.join(dir, "origin.git");
  git("clone", "--bare", checkout, origin);
  git("remote", "add", "origin", origin);
  fs.mkdirSync(api, { recursive: true });
  fs.mkdirSync(path.join(app, "komputer-session"));
  fs.writeFileSync(path.join(api, "server.mjs"), "// fixture\n");
  fs.writeFileSync(path.join(api, "KEYS.txt"), "api-key: test-only\n");
  fs.writeFileSync(path.join(app, "komputer-session", ".mcp.json"), '{"fixture":true}\n');
  git("add", ".");
  git("commit", "-m", "fixture komputer layout");
  const current = git("rev-parse", "HEAD");
  fs.writeFileSync(path.join(app, ".deploy-prev"), previous);
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return { api, git, current };
}

for (const script of ["remote-swap.sh", "remote-rollback.sh"]) {
  test(`${script} refuses a pre-migration revision before resetting the checkout`, (t) => {
    if (spawnSync(bash, ["--version"]).error) return t.skip("bash is not installed");
    const f = deploymentFixture(t);
    const r = spawnSync(bash, ["-s", "--", f.api.replaceAll("\\", "/")], {
      input: fs.readFileSync(path.join(project, "deploy", script), "utf8"),
      env: { ...process.env, NODE_BIN: process.execPath.replaceAll("\\", "/") },
      encoding: "utf8", timeout: 10_000,
    });
    assert.equal(r.error, undefined);
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /not compatible with the komputer layout/);
    assert.equal(f.git("rev-parse", "HEAD"), f.current, "must not reset across the directory migration");
    assert.equal(fs.readFileSync(path.join(f.api, "KEYS.txt"), "utf8"), "api-key: test-only\n");
  });
}
