# Start the Kloud Kode Body on this computer.
#
# The service is the Go binary built from Komputer/cmd/kloud-kode. It is built
# here on first start and whenever a source file is newer than the binary.
# Set KOMPUTER_IMPL=node to run the previous JavaScript implementation instead.
$ErrorActionPreference = "Stop"
$Dir = Split-Path -Parent $MyInvocation.MyCommand.Path
$Module = Split-Path -Parent $Dir
$Bin = Join-Path $Dir "kloud-kode.exe"
$Mode = if ($args.Count -ge 1) { $args[0] } else { "http" }

if ($env:KOMPUTER_IMPL -eq "node") {
  $node = if ($env:NODE_BIN) { $env:NODE_BIN } else { (Get-Command node -ErrorAction SilentlyContinue).Source }
  if (-not $node) { throw "node not found (set NODE_BIN)" }
  & $node (Join-Path $Dir "server.mjs") $Mode
  exit $LASTEXITCODE
}

$Go = if ($env:GO_BIN) { $env:GO_BIN } else { (Get-Command go -ErrorAction SilentlyContinue).Source }

$stale = -not (Test-Path $Bin)
if (-not $stale) {
  $binTime = (Get-Item $Bin).LastWriteTimeUtc
  $newer = Get-ChildItem (Join-Path $Module "cmd"), (Join-Path $Module "internal") -Recurse -Filter *.go |
    Where-Object { $_.LastWriteTimeUtc -gt $binTime } | Select-Object -First 1
  if ($newer) { $stale = $true }
  elseif ((Get-Item (Join-Path $Module "go.mod")).LastWriteTimeUtc -gt $binTime) { $stale = $true }
}
if ($stale) {
  if (-not $Go) { throw "go not found and $Bin is missing or stale. Install Go 1.27+ or set GO_BIN" }
  Write-Host "building $Bin"
  Push-Location $Module
  try { & $Go build -trimpath -o $Bin ./cmd/kloud-kode } finally { Pop-Location }
  if ($LASTEXITCODE -ne 0) { throw "go build failed" }
}
Push-Location $Dir
try { & $Bin body $Mode } finally { Pop-Location }
exit $LASTEXITCODE
