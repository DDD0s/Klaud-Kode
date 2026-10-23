# Start the Kloud Kode Body on this computer.
# Runs a checksum-verified precompiled release; no Go toolchain is needed.
# KLOUD_KODE_BUILD=1 builds from this checkout instead.
$ErrorActionPreference = "Stop"
$Dir = Split-Path -Parent $MyInvocation.MyCommand.Path
$Module = Split-Path -Parent $Dir
$Mode = if ($args.Count -ge 1) { $args[0] } else { "http" }

if ($env:KOMPUTER_IMPL -eq "node") {
  $node = if ($env:NODE_BIN) { $env:NODE_BIN } else { (Get-Command node -ErrorAction SilentlyContinue).Source }
  if (-not $node) { throw "node not found (set NODE_BIN)" }
  & $node (Join-Path $Dir "server.mjs") $Mode
  exit $LASTEXITCODE
}

function Get-Sha256([string] $Path) {
  # Get-FileHash exists in PowerShell 4+, but 5.1 on some hosts lacks it.
  if (Get-Command Get-FileHash -ErrorAction SilentlyContinue) {
    return (Get-FileHash $Path -Algorithm SHA256).Hash.ToLower()
  }
  $sha = [Security.Cryptography.SHA256]::Create()
  try { return (($sha.ComputeHash([IO.File]::ReadAllBytes($Path)) | ForEach-Object { $_.ToString("x2") }) -join "") }
  finally { $sha.Dispose() }
}

function Prepare-Binary {
  if ($env:KLOUD_KODE_BIN) {
    if (-not (Test-Path $env:KLOUD_KODE_BIN)) { throw "KLOUD_KODE_BIN does not exist: $env:KLOUD_KODE_BIN" }
    return (Resolve-Path $env:KLOUD_KODE_BIN).Path
  }
  $version = $env:KLOUD_KODE_VERSION
  if (-not $version -and (Test-Path (Join-Path $Module "VERSION"))) { $version = (Get-Content (Join-Path $Module "VERSION") -Raw).Trim() }
  $repo = if ($env:KLOUD_KODE_REPO) { $env:KLOUD_KODE_REPO } else { "DDD0s/Kloud-Kode" }
  if (-not $version -or $version -eq "latest") {
    $version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest" -UseBasicParsing).tag_name
  }
  $arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
  $binDir = Join-Path $Dir "bin"
  $bin = Join-Path $binDir "kloud-kode-$version.exe"
  if (Test-Path $bin) { return $bin }
  if ($env:KLOUD_KODE_BUILD -eq "1") {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw "KLOUD_KODE_BUILD=1 needs go on PATH" }
    Write-Host "building $bin from source"
    Push-Location $Module
    try { & go build -trimpath -ldflags "-X github.com/DDD0s/Kloud-Kode/Komputer/internal/version.Value=$version" -o $bin ./cmd/kloud-kode } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
    return $bin
  }
  $base = if ($env:KLOUD_KODE_RELEASE_BASE) { $env:KLOUD_KODE_RELEASE_BASE } else { "https://github.com/$repo/releases" }
  $asset = "kloud-kode-$version-windows-$arch.zip"
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Force $tmp | Out-Null
  try {
    Write-Host "downloading Kloud Kode $version (windows/$arch)"
    Invoke-WebRequest "$base/download/$version/$asset" -OutFile (Join-Path $tmp $asset) -UseBasicParsing
    Invoke-WebRequest "$base/download/$version/SHA256SUMS" -OutFile (Join-Path $tmp "SHA256SUMS") -UseBasicParsing
    $expected = ((Get-Content (Join-Path $tmp "SHA256SUMS") | Where-Object { $_ -match [regex]::Escape($asset) }) -split "\s+")[0]
    $actual = Get-Sha256 (Join-Path $tmp $asset)
    if (-not $expected) { throw "SHA256SUMS has no entry for $asset" }
    if ($actual -ne $expected.ToLower()) { throw "checksum mismatch for $asset" }
    Write-Host "$asset`: OK"
    Expand-Archive (Join-Path $tmp $asset) (Join-Path $tmp "extract") -Force
    New-Item -ItemType Directory -Force $binDir | Out-Null
    Copy-Item (Join-Path $tmp "extract\kloud-kode.exe") $bin -Force
    & $bin version | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "the downloaded binary does not run" }
  } finally { Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue }
  return $bin
}

$Bin = Prepare-Binary
Push-Location $Dir
try { & $Bin body $Mode } finally { Pop-Location }
exit $LASTEXITCODE
