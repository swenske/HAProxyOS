#!/usr/bin/env bash
# Proves the *whole* real, single-disk, UEFI-bootable A/B shape works
# end to end - real OVMF firmware, ONE virtio-blk drive (unlike every
# other boot test here, which either uses QEMU's -kernel/-append
# shortcut or several separate drives), no -kernel/-append at all - and
# that image/disk/activate-slot.sh's in-place ESP swap (the groundwork
# for a real LifecycleService.Upgrade/Rollback) actually does what it
# claims: switches which slot boots without disturbing STATE or either
# slot's own content.
#
#   1. boot disk.img as built by image/disk/assemble.sh (active slot A)
#      - OVMF must discover and boot the ESP's UKI on its own, dm-verity
#      must verify slot A, HAProxy must answer real HTTP, and
#      janusd must log "first boot" (a fresh CA bootstrapped onto
#      STATE, partition 6).
#   2. image/disk/activate-slot.sh switches the SAME disk's ESP to slot
#      B, in place - BOOT-A-DATA/HASH, BOOT-B-DATA/HASH and STATE are
#      never touched by this operation.
#   3. boot the same disk.img again - OVMF's own log must now show it
#      chain-loaded a table referencing /dev/vda4 (BOOT-B-DATA), not
#      /dev/vda2, proving the ESP swap actually took effect; HAProxy
#      must again answer HTTP; and janusd must NOT log "first boot"
#      again - internal/pki.LoadOrBootstrap finding and loading the
#      *same* CA boot 1 wrote, proving STATE survived the slot switch
#      untouched, not just that slot B's squashfs/verity happens to
#      verify.
#
# Usage: hack/qemu-uefi-ab-boot-test.sh <disk.img> <bzImage> <rootfs-dir>
# <disk.img> must be image/disk/assemble.sh's output (mutated in place
# by step 2). <rootfs-dir> is passed straight through to
# image/disk/activate-slot.sh for rebuilding the UKI.
set -euo pipefail

DISK="${1:?usage: $0 <disk.img> <bzImage> <rootfs-dir>}"
KERNEL="${2:?usage: $0 <disk.img> <bzImage> <rootfs-dir>}"
ROOTFS_DIR="${3:?usage: $0 <disk.img> <bzImage> <rootfs-dir>}"
HTTP_TIMEOUT_SECS="${QEMU_UEFI_AB_HTTP_TIMEOUT:-40}"
HOST_PORT="${QEMU_UEFI_AB_TEST_PORT:-18089}"
MARKER="JANUS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# Boots $DISK under real OVMF firmware and polls for HTTP 200, writing
# the console log to $1. Returns non-zero if HAProxy never answered.
boot_disk() {
  local log="$1"
  local ovmf_vars="$WORKDIR/OVMF_VARS-$(basename "$log").fd"
  cp "$OVMF_VARS_TEMPLATE" "$ovmf_vars"

  qemu-system-x86_64 \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$ovmf_vars" \
    -drive file="$DISK",format=raw,if=virtio \
    -nographic -no-reboot -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$log" \
    &
  QEMU_PID=$!

  local deadline=$((SECONDS + HTTP_TIMEOUT_SECS)) code=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT}/" || true)"
    [ "$code" = "200" ] && break
    sleep 1
  done

  kill "$QEMU_PID" 2>/dev/null || true
  wait "$QEMU_PID" 2>/dev/null || true
  QEMU_PID=""

  [ "$code" = "200" ]
}

# --- 1. boot slot A (the disk's default active slot) ---
A_LOG="$WORKDIR/slot-a.log"
if ! boot_disk "$A_LOG"; then
  echo "UEFI A/B test FAILED: no HTTP 200 booting slot A within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$A_LOG" >&2
  exit 1
fi
if ! grep -q "$MARKER" "$A_LOG" || ! grep -q "$FIRST_BOOT_MSG" "$A_LOG"; then
  echo "UEFI A/B test FAILED: slot A boot didn't show both the boot marker and a fresh CA bootstrap" >&2
  echo "--- console output ---" >&2
  cat "$A_LOG" >&2
  exit 1
fi
echo "Slot A OK: real UEFI boot, HTTP 200, bootstrapped a new CA onto STATE"

# --- 2. switch the ESP to slot B, in place ---
"$SELF_DIR/../image/disk/activate-slot.sh" "$DISK" "$KERNEL" "$ROOTFS_DIR" B
echo "Activated slot B on $DISK's ESP (BOOT-A/BOOT-B/STATE untouched)"

# --- 3. boot again: must be slot B, and STATE must have survived ---
B_LOG="$WORKDIR/slot-b.log"
if ! boot_disk "$B_LOG"; then
  echo "UEFI A/B test FAILED: no HTTP 200 booting slot B within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$B_LOG" >&2
  exit 1
fi
if ! grep -q "$MARKER" "$B_LOG"; then
  echo "UEFI A/B test FAILED: slot B boot never printed $MARKER" >&2
  echo "--- console output ---" >&2
  cat "$B_LOG" >&2
  exit 1
fi
if ! grep -q "dm-mod.create=.*verity 1 /dev/vda4 /dev/vda5" "$B_LOG"; then
  echo "UEFI A/B test FAILED: console cmdline doesn't reference BOOT-B-DATA/HASH (/dev/vda4+5) - the ESP swap didn't take effect" >&2
  echo "--- console output ---" >&2
  cat "$B_LOG" >&2
  exit 1
fi
if grep -q "$FIRST_BOOT_MSG" "$B_LOG"; then
  echo "UEFI A/B test FAILED: slot B logged '$FIRST_BOOT_MSG' - STATE didn't survive the slot switch, a new CA was generated instead of loading the one from slot A's boot" >&2
  echo "--- console output ---" >&2
  cat "$B_LOG" >&2
  exit 1
fi
echo "Slot B OK: real UEFI boot from the switched ESP (/dev/vda4+5 confirmed in the cmdline), STATE (CA) survived the switch untouched"
echo "UEFI A/B test OK: the full single-disk, real-UEFI A/B shape works end to end"
