#!/usr/bin/env bash
# Proves image/disk/assemble.sh's single GPT disk image actually has two
# independently bootable A/B slots - not just that the partition table
# looks right. Boots the SAME disk.img twice, attached as ONE virtio-blk
# drive (unlike hack/qemu-verity-boot-test.sh's separate-drives harness):
# once with the kernel's "dm-mod.create=" cmdline pointed at
# BOOT-A-DATA/BOOT-A-HASH (partitions 2/3 - /dev/vda2, /dev/vda3; ESP is
# partition 1, GPT scanning via CONFIG_EFI_PARTITION, already on
# unconditionally by default - no separate "enable GPT" kernel config
# needed), once at BOOT-B-DATA/BOOT-B-HASH (partitions 4/5 - /dev/vda4,
# /dev/vda5). Both boots must serve real HTTP, same as
# qemu-verity-boot-test.sh's own "good" boot.
#
# Both slots hold the same content today (see image/disk/assemble.sh -
# there's no LifecycleService.Upgrade yet to install something
# different into the inactive slot), so this specifically proves the
# on-disk layout/dm-verity-via-partition-device mechanics work for
# either slot, not a real upgrade/rollback workflow.
#
# A third boot, back on slot A, then proves rootfs/init/main.go's
# resolveStateDevice (cmdline.go) correctly finds STATE (partition 6)
# on *this* real single-disk layout, not just the older separate-drive
# one hack/qemu-state-persist-test.sh already covers: janusd's
# "first boot" log line must NOT reappear, since boot 1 already
# bootstrapped a CA onto partition 6 - internal/pki.LoadOrBootstrap
# should find and load it instead. The disk isn't attached read-only
# for this reason: dm-verity's own "ro" flag in dm-mod.create= already
# protects the verity-mapped root regardless of whether the underlying
# block device itself is writable, so STATE (an ordinary, unprotected
# partition) can be written to without weakening that at all.
#
# Usage: hack/qemu-ab-boot-test.sh <bzImage> <rootfs-dir>
# <rootfs-dir> must contain disk.img (image/disk/assemble.sh) and
# rootfs.verity.info/rootfs.roothash (rootfs/assemble.sh) - both slots
# were built from the same rootfs.squashfs/rootfs.verity, so one root
# hash covers both.
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <rootfs-dir>}"
ROOTFS_DIR="${2:?usage: $0 <bzImage> <rootfs-dir>}"
HTTP_TIMEOUT_SECS="${QEMU_AB_HTTP_TIMEOUT:-30}"
HOST_PORT="${QEMU_AB_TEST_PORT:-18085}"

DISK="$ROOTFS_DIR/disk.img"

dm_table() {
  # $1 = data partition device, $2 = hash partition device
  "$(dirname "$0")/dm-verity-cmdline.sh" "$ROOTFS_DIR" "$1" "$2"
}

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# Boots disk.img with dm-mod.create= pointed at the given data/hash
# partition devices and polls for HTTP 200, writing the console log to
# $3. Returns non-zero if HAProxy never answered within the timeout.
boot_slot() {
  local data_dev="$1" hash_dev="$2" log="$3"
  qemu-system-x86_64 \
    -kernel "$KERNEL" \
    -append "console=ttyS0 panic=-1 dm-mod.create=\"$(dm_table "$data_dev" "$hash_dev")\" root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp" \
    -nographic -no-reboot -display none -m 256M \
    -drive file="$DISK",format=raw,if=virtio \
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

FIRST_BOOT_MSG="pki: first boot - generated a new CA"

A_LOG="$WORKDIR/slot-a.log"
if ! boot_slot /dev/vda2 /dev/vda3 "$A_LOG"; then
  echo "A/B boot test FAILED: slot A (BOOT-A-DATA/BOOT-A-HASH, /dev/vda2+3) never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$A_LOG" >&2
  exit 1
fi
if ! grep -q "$FIRST_BOOT_MSG" "$A_LOG"; then
  echo "A/B boot test FAILED: slot A's first boot didn't log '$FIRST_BOOT_MSG' - expected a fresh bootstrap against a blank STATE partition" >&2
  echo "--- console output ---" >&2
  cat "$A_LOG" >&2
  exit 1
fi
echo "Slot A OK: HAProxy answered HTTP 200, booted from BOOT-A-DATA/BOOT-A-HASH (/dev/vda2+3), bootstrapped a new CA onto STATE"

B_LOG="$WORKDIR/slot-b.log"
if ! boot_slot /dev/vda4 /dev/vda5 "$B_LOG"; then
  echo "A/B boot test FAILED: slot B (BOOT-B-DATA/BOOT-B-HASH, /dev/vda4+5) never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$B_LOG" >&2
  exit 1
fi
echo "Slot B OK: HAProxy answered HTTP 200, booted from BOOT-B-DATA/BOOT-B-HASH (/dev/vda4+5)"

A2_LOG="$WORKDIR/slot-a-2.log"
if ! boot_slot /dev/vda2 /dev/vda3 "$A2_LOG"; then
  echo "A/B boot test FAILED: slot A's second boot never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$A2_LOG" >&2
  exit 1
fi
if grep -q "$FIRST_BOOT_MSG" "$A2_LOG"; then
  echo "A/B boot test FAILED: slot A's second boot logged '$FIRST_BOOT_MSG' again - resolveStateDevice isn't finding STATE (partition 6) on this real single-disk layout" >&2
  echo "--- console output ---" >&2
  cat "$A2_LOG" >&2
  exit 1
fi
echo "Slot A (2nd boot) OK: loaded the CA from STATE instead of regenerating it - resolveStateDevice finds partition 6 correctly"

echo "A/B boot test OK: both slots of the single GPT disk image are independently bootable, and STATE persists across a reboot on this real layout"
