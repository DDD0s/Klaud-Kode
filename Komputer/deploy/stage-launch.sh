#!/usr/bin/env bash
# Run a command inside the staged identity of the caller computer.
#
# Usage: stage-launch.sh <stage-dir> <command> [args...]
#
# The stage directory comes from "kloud-kode identity stage". This script
# enters fresh namespaces and lays the staged files over the real ones, so
# every probe the command makes -- hostname, /etc/machine-id, /etc/os-release,
# /proc/cpuinfo, /proc/meminfo, /sys/class/dmi, /etc/localtime -- answers with
# the caller computer, not with this server. The kernel, CPUID and network
# egress stay the server computer.
set -euo pipefail

stage=${1:?usage: stage-launch.sh <stage-dir> <command> [args...]}
shift

[ -d "$stage" ] || { echo "stage-launch: no such stage dir: $stage" >&2; exit 1; }
[ -f "$stage/etc/hostname" ] || { echo "stage-launch: $stage is not a stage (missing etc/hostname)" >&2; exit 1; }
command -v unshare >/dev/null || { echo "stage-launch: unshare(1) is required" >&2; exit 1; }

hostname=$(cat "$stage/etc/hostname")
home_user=$(basename "$(find "$stage/home" -mindepth 1 -maxdepth 1 -type d | head -n1)")
stage_abs=$(cd "$stage" && pwd)

INNER='# The mounts must run in the same namespace as the command, after unshare.
set -eu
stage=$1; shift
hostname=$1; shift
mount --make-rprivate / 2>/dev/null || true
hostname "$hostname" 2>/dev/null || true
for pair in \
  "etc/hostname:/etc/hostname" \
  "etc/machine-id:/etc/machine-id" \
  "etc/os-release:/etc/os-release" \
  "etc/locale.conf:/etc/locale.conf" \
  "etc/hosts:/etc/hosts" \
  "proc/cpuinfo:/proc/cpuinfo" \
  "proc/meminfo:/proc/meminfo"
do
  src="$stage/${pair%%:*}"; dst="${pair##*:}"
  [ -f "$src" ] && mount --bind "$src" "$dst" 2>/dev/null || true
done
if [ -d "$stage/sys/class/dmi/id" ]; then
  for f in "$stage/sys/class/dmi/id/"*; do
    name=$(basename "$f")
    [ -e "/sys/class/dmi/id/$name" ] && mount --bind "$f" "/sys/class/dmi/id/$name" 2>/dev/null || true
  done
fi
if [ -f "$stage/etc/timezone" ]; then
  tz=$(cat "$stage/etc/timezone")
  if [ -e "/usr/share/zoneinfo/$tz" ]; then
    mount --bind "/usr/share/zoneinfo/$tz" /etc/localtime 2>/dev/null || true
  fi
fi
if [ -n "${KL_STAGE_HOME:-}" ] && [ -d "$stage/home/$KL_STAGE_HOME" ]; then
  mkdir -p "/home/$KL_STAGE_HOME"
  mount --bind "$stage/home/$KL_STAGE_HOME" "/home/$KL_STAGE_HOME" 2>/dev/null || true
fi
export HOSTNAME="$hostname"
[ -f "$stage/stage.env" ] && set -a && . "$stage/stage.env" && set +a
exec "$@"
'

KL_STAGE_HOME="$home_user" exec unshare --mount --uts --fork --pid bash -c "$INNER" _ "$stage_abs" "$hostname" "$@"
