// agy runs on a Linux host but its built-in tools act on the user's computer,
// which may run Windows. These helpers translate between the two views:
// Windows paths <-> the host-side mount of the Body root, line endings, the
// shell the model should write for, and a fallback for bash it writes anyway.
import path from "node:path";

const win = path.win32;

/** C:\x, C:/x, C: and \\server\share paths. */
export const isWindowsPath = (p) => typeof p === "string" && (/^[A-Za-z]:([\\/]|$)/.test(p) || /^\\\\[^\\]/.test(p));

const trimSep = (p) => (/^[A-Za-z]:\\$/.test(p) ? p : p.replace(/[\\]+$/, ""));
const normWin = (p) => trimSep(win.normalize(p.replace(/\//g, "\\")));

/**
 * Where the Body root appears on this host when the user's computer runs Windows:
 * C:\Users\me\proj under <base>/c/Users/me/proj, the layout WSL uses under /mnt,
 * so with base /mnt the paths agy shows read as the familiar /mnt/c/... form.
 */
export function windowsMountDir(base, root) {
  const n = normWin(root);
  const unc = /^\\\\([^\\]+)\\(.*)$/.exec(n);
  const parts = unc ? ["unc", unc[1], ...unc[2].split("\\")] : [n[0].toLowerCase(), ...n.slice(3).split("\\")];
  return path.join(base, ...parts.filter(Boolean));
}

/** The host path under the mount for a Windows path inside the Body root, or null outside it. */
export function windowsToMount(p, { root, mount }) {
  if (!isWindowsPath(p) || !isWindowsPath(root) || !mount) return null;
  const target = normWin(p);
  const base = normWin(root);
  const lower = target.toLowerCase();
  const prefix = base.toLowerCase();
  if (lower !== prefix && !lower.startsWith(prefix.endsWith("\\") ? prefix : prefix + "\\")) return null;
  const rel = target.slice(base.length).replace(/^\\+/, "");
  return rel ? path.join(mount, ...rel.split("\\")) : mount;
}

/** The Windows path for a host path under the mount, or null outside it. */
export function mountToWindows(p, { root, mount }, sep = "\\") {
  if (!mount || !isWindowsPath(root) || typeof p !== "string") return null;
  const rel = path.relative(mount, path.resolve(p));
  if (rel.startsWith("..") || path.isAbsolute(rel)) return null;
  const base = normWin(root);
  const out = rel ? `${base.replace(/\\$/, "")}\\${rel.split(path.sep).join("\\")}` : base;
  return sep === "/" ? out.replace(/\\/g, "/") : out;
}

const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/** Mount paths in a command line rewritten to the user's Windows paths (sep "/" for Git Bash). */
export function commandToWindows(command, { root, mount }, sep = "\\") {
  if (!mount || !isWindowsPath(root)) return command;
  const re = new RegExp(`${escapeRe(mount)}(?=$|[/\\s"'\`;|&<>(),])((?:/[^\\s"'\`;|&<>(),]*)?)`, "g");
  return command.replace(re, (_, rest) => mountToWindows(mount + rest, { root, mount }, sep));
}

/** LF and CRLF line breaks as CRLF. */
export const toCrlf = (s) => s.replace(/\r?\n/g, "\r\n");

/** Streams Windows output with CRLF turned into LF, also across chunk boundaries. */
export function crlfToLf() {
  let carry = "";
  return {
    push(s) {
      const text = carry + s;
      carry = text.endsWith("\r") ? "\r" : "";
      return (carry ? text.slice(0, -1) : text).replace(/\r\n/g, "\n");
    },
    end() {
      const rest = carry;
      carry = "";
      return rest;
    },
  };
}

const PS_HINT = /\b(Get|Set|New|Remove|Select|Where|ForEach|Write|Test|Invoke|Start|Stop|Copy|Move|Out|Format|Measure|Sort|Join|Split|Resolve|Expand|Compress|ConvertTo|ConvertFrom|Add|Clear|Import|Export|Rename|Push|Pop)-[A-Z][A-Za-z]+\b|\$env:|\$PSVersionTable|\$LASTEXITCODE|\[System\.|\[IO\./;
const SEG = String.raw`(?:^|[;&|(]\s*)`;
const POSIX_HINTS = [
  new RegExp(`${SEG}(ls|rm|cp|mv|mkdir|cat|grep|head|tail|chmod|chown|touch|uname|sed|awk|xargs|wc|du|df|ps|kill|ln)\\s+-{1,2}[A-Za-z]`),
  new RegExp(`${SEG}(grep|sed|awk|xargs|chmod|chown|uname|touch|which|head|tail|wc|source|export|ln)\\b`),
  /\/dev\/null/,
  /<<-?\s*['"]?\w+/,
  /\bif\s+\[\[?\s|;\s*then\b|\bfi\s*($|;)|;\s*do\b|\bdone\s*($|;)/,
  /(^|\s)~\/|\$HOME\b|\$\{[A-Za-z_]+\}/,
];

/** Whether a command line is POSIX shell syntax rather than PowerShell. */
export function looksPosix(command) {
  if (PS_HINT.test(command)) return false;
  return POSIX_HINTS.some((re) => re.test(command));
}

/** Split on ; and && outside quotes: [{ text, op }] where op joins it to the previous one. */
function segments(command) {
  const out = [];
  let cur = "";
  let quote = null;
  let op = null;
  for (let i = 0; i < command.length; i++) {
    const c = command[i];
    if (quote) {
      if (c === quote) quote = null;
      cur += c;
    } else if (c === "'" || c === '"') {
      quote = c;
      cur += c;
    } else if (c === ";" || c === "\n") {
      out.push({ text: cur.trim(), op });
      cur = "";
      op = ";";
    } else if (c === "&" && command[i + 1] === "&") {
      out.push({ text: cur.trim(), op });
      cur = "";
      op = "&&";
      i++;
    } else cur += c;
  }
  if (quote) return null;
  out.push({ text: cur.trim(), op });
  return out.filter((s) => s.text);
}

/** Words of one simple command, or null for anything but plain words and quotes. */
function words(text) {
  const out = [];
  let cur = null;
  let quote = null;
  for (const c of text) {
    if (quote) {
      if (c === quote) quote = null;
      else if (quote === '"' && (c === "$" || c === "`" || c === "\\")) return null;
      else cur += c;
    } else if (c === "'" || c === '"') {
      quote = c;
      cur ??= "";
    } else if (/\s/.test(c)) {
      if (cur !== null) out.push(cur);
      cur = null;
    } else if (/[|<>$`\\(){}]/.test(c)) return null;
    else cur = (cur ?? "") + c;
  }
  if (quote) return null;
  if (cur !== null) out.push(cur);
  return out;
}

const lit = (s) => `'${s.replace(/'/g, "''")}'`;
const pathArg = (p) => (/[*?[]/.test(p) || p.startsWith("~") ? `-Path ${lit(p)}` : `-LiteralPath ${lit(p)}`);
const paths = (ps) => (ps.some((p) => /[*?[]/.test(p) || p.startsWith("~")) ? `-Path ${ps.map(lit).join(",")}` : `-LiteralPath ${ps.map(lit).join(",")}`);

/** [flags, operands]: -la -> "la"; a bare "-" or "--" ends the flags. */
function flagsOf(args) {
  let flags = "";
  const rest = [];
  let done = false;
  for (const a of args) {
    if (!done && a === "--") done = true;
    else if (!done && /^-[A-Za-z0-9]+$/.test(a)) flags += a.slice(1);
    else rest.push(a);
  }
  return [flags, rest];
}

const only = (flags, allowed) => [...flags].every((f) => allowed.includes(f));

function translateOne(text) {
  let t = text.replace(/&>\s*\/dev\/null/g, " *>$$null").replace(/(\d?)>\s*\/dev\/null/g, " $1>$$null").replace(/2>&1/g, " 2>&1");
  const redirect = [];
  t = t.replace(/\s(\*|\d)?>\$null|\s2>&1/g, (m) => {
    redirect.push(m.trim());
    return " ";
  });
  const w = words(t.trim());
  if (!w || !w.length) return null;
  const [cmd, ...args] = w;
  const [flags, ops] = flagsOf(args);
  const tail = redirect.length ? " " + redirect.join(" ") : "";
  const ok = (s) => s + tail;
  switch (cmd) {
    case "ls":
    case "dir":
      if (!only(flags, "laRh1AF")) return null;
      return ok(["Get-ChildItem", /[aA]/.test(flags) && "-Force", flags.includes("R") && "-Recurse", flags.includes("1") && "-Name", ops.length && paths(ops)].filter(Boolean).join(" "));
    case "pwd":
      return !args.length ? ok("(Get-Location).Path") : null;
    case "cat":
      return !flags && ops.length ? ok(`Get-Content ${paths(ops)}`) : null;
    case "head":
    case "tail": {
      let n = 10;
      const rest = [];
      for (let i = 0; i < args.length; i++) {
        if (args[i] === "-n" && /^\d+$/.test(args[i + 1] || "")) n = Number(args[++i]);
        else if (/^-n\d+$/.test(args[i]) || /^-\d+$/.test(args[i])) n = Number(args[i].replace(/^-n?/, ""));
        else if (args[i].startsWith("-")) return null;
        else rest.push(args[i]);
      }
      if (rest.length !== 1) return null;
      return ok(`Get-Content ${pathArg(rest[0])} ${cmd === "head" ? "-TotalCount" : "-Tail"} ${n}`);
    }
    case "mkdir":
      return only(flags, "pv") && ops.length ? ok(`New-Item -ItemType Directory -Force -Path ${ops.map(lit).join(",")} | Out-Null`) : null;
    case "rm":
      if (!only(flags, "rRfv") || !ops.length) return null;
      return ok(["Remove-Item", paths(ops), /[rR]/.test(flags) && "-Recurse", flags.includes("f") && "-Force -ErrorAction SilentlyContinue"].filter(Boolean).join(" "));
    case "cp":
      if (!only(flags, "rRfapv") || ops.length !== 2) return null;
      return ok(`Copy-Item ${pathArg(ops[0])} -Destination ${lit(ops[1])}${/[rRa]/.test(flags) ? " -Recurse" : ""} -Force`);
    case "mv":
      return only(flags, "fv") && ops.length === 2 ? ok(`Move-Item ${pathArg(ops[0])} -Destination ${lit(ops[1])} -Force`) : null;
    case "touch":
      if (flags || ops.length !== 1) return null;
      return ok(`if (Test-Path -LiteralPath ${lit(ops[0])}) { (Get-Item -LiteralPath ${lit(ops[0])}).LastWriteTime = Get-Date } else { New-Item -ItemType File -Path ${lit(ops[0])} | Out-Null }`);
    case "which":
      return !flags && ops.length === 1 ? ok(`(Get-Command ${lit(ops[0])} -ErrorAction Stop).Source`) : null;
    case "uname":
      return only(flags, "asrmno") ? ok("[System.Environment]::OSVersion.VersionString") : null;
    case "env":
    case "printenv":
      if (flags) return null;
      if (cmd === "printenv" && ops.length === 1 && /^\w+$/.test(ops[0])) return ok(`$env:${ops[0]}`);
      return !ops.length ? ok('Get-ChildItem Env: | ForEach-Object { "$($_.Name)=$($_.Value)" }') : null;
    case "grep": {
      if (!only(flags, "rRniIlEFHsw") || !ops.length) return null;
      const [pattern, ...where] = ops;
      const ps = [`-Pattern ${lit(pattern)}`, !/i/.test(flags) && "-CaseSensitive", flags.includes("F") && "-SimpleMatch", flags.includes("l") && "-List"].filter(Boolean).join(" ");
      const src = /[rR]/.test(flags)
        ? `Get-ChildItem ${where.length ? paths(where) : "-Path ."} -Recurse -File | Select-String ${ps}`
        : where.length ? `Select-String ${ps} ${paths(where)}` : null;
      return src ? ok(src + (flags.includes("l") ? " | ForEach-Object Path" : "")) : null;
    }
    case "wc":
      return flags === "l" && ops.length === 1 ? ok(`(Get-Content ${pathArg(ops[0])} | Measure-Object -Line).Lines`) : null;
    case "export": {
      const m = ops.length === 1 && !flags ? /^([A-Za-z_]\w*)=(.*)$/.exec(ops[0]) : null;
      return m ? ok(`$env:${m[1]} = ${lit(m[2])}`) : null;
    }
    case "echo":
      return !flags ? ok(`Write-Output ${ops.length ? ops.map(lit).join(" ") : "''"}`) : null;
    case "true":
      return !args.length ? ok("$null") : null;
    default:
      return null;
  }
}

/**
 * A PowerShell equivalent of a simple POSIX command line (ls -la, mkdir -p, rm -rf,
 * cat, head, tail, grep -rn, ...; joined by ; or &&), or null when it is anything more.
 * Windows PowerShell 5.1 has no &&, so `a && b` becomes `a; if ($?) { b }`.
 */
export function translatePosix(command) {
  const segs = segments(command.trim());
  if (!segs?.length) return null;
  const parts = [];
  for (const s of segs) {
    const t = translateOne(s.text);
    if (t === null) return null;
    parts.push({ text: t, op: s.op });
  }
  let out = "";
  let open = 0;
  for (const p of parts) {
    if (!out) out = p.text;
    else if (p.op === "&&") {
      out += `; if ($?) { ${p.text}`;
      open++;
    } else out += `; ${p.text}`;
  }
  return out + " }".repeat(open);
}

const SHELL_LABEL = {
  powershell: "Windows PowerShell 5.1 (powershell.exe)",
  pwsh: "PowerShell 7 (pwsh.exe)",
  cmd: "cmd.exe",
  default: "cmd.exe",
  bash: "Git Bash",
};

/**
 * What the model needs to know about the user's computer when it is not the Linux
 * host agy describes in its own system prompt ("The USER's OS version is linux",
 * run_command's "Operating System: linux. Shell: bash", the workspace path).
 */
export function environmentNote({ platform, osVersion, release, arch, root, mount, shell, files, posixFallback, bash }) {
  if (platform !== "win32") {
    if (!platform || platform === "linux") return "";
    const os = platform === "darwin" ? "macOS" : platform;
    return [
      "<user_computer>",
      `agy's own notes ("The USER's OS version is linux", "Operating System: linux") describe the host that runs agy, not the user's computer. run_command and the file tools act on the user's computer, which runs ${os}${osVersion ? ` (${osVersion}${release ? ` ${release}` : ""})` : ""}; write commands for ${os}.`,
      "</user_computer>",
    ].join("\n");
  }
  const label = SHELL_LABEL[shell] || shell;
  const ps = shell === "powershell" || shell === "pwsh";
  const lines = [
    "<user_computer>",
    `agy's own notes ("The USER's OS version is linux", run_command's "Operating System: linux. Shell: bash", and the workspace ${mount}) describe the host that runs agy, not the user's computer. run_command and the file tools act on the user's computer, which runs Windows${osVersion ? ` (${osVersion}${release ? ` ${release}` : ""}${arch ? `, ${arch}` : ""})` : ""}:`,
    `- Workspace: ${root}. ${mount} is the same folder as agy sees it; prefer the Windows path.`,
    `- run_command runs ${label} in ${root}.` + (ps
      ? ` Write PowerShell, not bash: Get-ChildItem, Get-Content, Set-Content, Select-String, Remove-Item -Recurse -Force, New-Item -ItemType Directory -Force, $env:NAME.${shell === "powershell" ? " Windows PowerShell 5.1 has no && or ||: use `;` or `if ($?) { ... }`." : ""} For a cmd built-in use cmd /c "...".`
      : " Write cmd.exe commands."),
    `- Paths: Windows paths such as ${root}\\src\\app.js, in commands and in the file tools.` +
      (files ? ` The file tools reach only files inside ${root}; for anything else use run_command.` : " The file tools cannot reach the user's files here: read and write them with run_command (Get-Content, Set-Content)."),
    "- Output is UTF-8. Existing files keep their own line endings (CRLF is fine).",
  ];
  if (ps && posixFallback !== "off") {
    lines.push(`- A POSIX command that slips through (ls -la, rm -rf, grep -rn) is ${posixFallback === "translate" || !bash ? "translated to PowerShell when it is simple" : "run in Git Bash"}, but PowerShell is the shell to write for.`);
  }
  lines.push("</user_computer>");
  return lines.join("\n");
}
