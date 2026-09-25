#!/usr/bin/env bash
# Proves the SystemService RPCs the dashboard's own single-node view
# needs (Memory, CPUInfo, LoadAvg, DiskStats, plus VersionResponse's new
# active_slot/kernel_version/go_version fields - internal/api/system.go
# and system_stats.go) actually return real, sane values from a real
# boot's /proc - not just that they compile or satisfy a mocked test.
#
# Reuses hack/qemu-lifecycle-rollback-test.sh's own pattern: boot
# disk.img (image/disk/assemble.sh, slot A) under real OVMF, extract PKI
# straight from the STATE partition via debugfs (a script can't watch a
# live console the way a human doing this for real would - see that
# script's own comment), then call `janusctl system info` over real
# mTLS and check the printed values are plausible:
#   - active slot: A (proves VersionResponse.active_slot's
#     internal/bootslot plumbing actually resolves on a real A/B boot,
#     not just that it degrades gracefully on a non-A/B one)
#   - kernel version matches a real X.Y.Z pattern (not empty, not a
#     literal hardcoded version this test would go stale against)
#   - memory total > 0 bytes
#   - at least one CPU reported
#   - load average line parses (three floats)
#   - at least one disk (vda, the virtio-blk root disk) with a nonzero
#     read count (the boot itself already did plenty of reads)
#
# Usage: hack/qemu-system-info-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL="${2:?usage: $0 <disk.img> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_SYSINFO_HTTP_TIMEOUT:-40}"
HOST_HTTP_PORT="${QEMU_SYSINFO_HTTP_PORT:-18095}"
HOST_GRPC_PORT="${QEMU_SYSINFO_GRPC_PORT:-18096}"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

OVMF_VARS="$WORKDIR/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
LOG="$WORKDIR/console.log"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_HTTP_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" != "200" ]; then
  echo "System info test FAILED: never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "Boot OK: real UEFI boot, HTTP 200"

# --- extract PKI straight from disk.img's STATE partition, same reasoning
# as hack/qemu-lifecycle-rollback-test.sh ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none

for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "System info test FAILED: couldn't extract pki/$f from disk.img's STATE partition" >&2; exit 1; }
done

OUT="$("$CTL" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" system info)"
echo "$OUT"

fail=0
check() {
  local desc="$1" pattern="$2"
  if ! echo "$OUT" | grep -qE "$pattern"; then
    echo "System info test FAILED: expected $desc (pattern: $pattern) - not found in output above" >&2
    fail=1
  fi
}

check "active slot A" '^active slot: A$'
check "a real X.Y.Z kernel version" '^kernel version: [0-9]+\.[0-9]+\.[0-9]+$'
check "a nonempty go version" '^go version: go[0-9]'
check "at least 1 CPU" '^cpus: [1-9][0-9]*$'
check "a parseable load average" '^load average: [0-9]+\.[0-9]+ [0-9]+\.[0-9]+ [0-9]+\.[0-9]+$'
check "at least one disk" '^disks: [1-9][0-9]*$'
check "vda with real read activity" '^  vda: reads=[1-9][0-9]* writes='

# memory total > 0 needs actual arithmetic, not just a regex.
mem_total="$(echo "$OUT" | grep -oE 'total=[0-9]+' | grep -oE '[0-9]+')"
if [ -z "$mem_total" ] || [ "$mem_total" -le 0 ]; then
  echo "System info test FAILED: memory total_bytes was 0 or missing (got '${mem_total:-<none>}')" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "System info test OK: Memory/CPUInfo/LoadAvg/DiskStats and VersionResponse's active_slot/kernel_version/go_version all returned real, sane values from a real boot"
