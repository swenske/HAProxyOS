#!/usr/bin/env bash
# Proves image/uki/assemble.sh's Unified Kernel Image actually boots
# under *real* UEFI firmware (OVMF/edk2) - not QEMU's own `-kernel`/
# `-append` shortcut, which every other boot test in this project uses
# and which bypasses the whole boot chain this test exists to exercise.
# No `-kernel`, no `-append`, no `-initrd` at all: OVMF discovers the
# ESP (image/uki/esp-image.sh), loads \EFI\BOOT\BOOTX64.EFI (the UEFI
# spec's removable-media fallback path - no NVRAM boot entry needed),
# which is systemd-stub with the kernel and its exact cmdline embedded,
# which chain-loads into the kernel's own EFI stub via the handover
# protocol. From there it's the same as hack/qemu-verity-boot-test.sh's
# "good" boot: dm-verity assembles and verifies root from the two
# virtio-blk drives the cmdline was built to expect, real init runs,
# HAProxy must answer real HTTP.
#
# The ESP ends up as the *first* virtio-blk drive (vda) - unlike every
# other boot test, root's data/hash devices are /dev/vdb and /dev/vdc,
# not /dev/vda and /dev/vdb (caught by a first attempt at this exact
# test: OVMF and the kernel both booted fine, dm-verity even assembled
# /dev/dm-0 successfully, but against the ESP's own FAT12/16/32
# metadata instead of the real squashfs - "metadata block 1 is
# corrupted" - because the UKI's baked-in cmdline still said
# /dev/vda /dev/vdb from before the ESP was added to the boot).
#
# Usage: hack/qemu-uefi-boot-test.sh <rootfs-dir> <esp.img>
# <rootfs-dir> must contain rootfs.squashfs and rootfs.verity (see
# rootfs/assemble.sh) - the UKI's own cmdline already has the root hash
# baked in, this script doesn't need rootfs.roothash/verity.info itself.
set -euo pipefail

ROOTFS_DIR="${1:?usage: $0 <rootfs-dir> <esp.img>}"
ESP="${2:?usage: $0 <rootfs-dir> <esp.img>}"
HTTP_TIMEOUT_SECS="${QEMU_UEFI_HTTP_TIMEOUT:-40}"
HOST_PORT="${QEMU_UEFI_TEST_PORT:-18086}"
MARKER="JANUS_INIT_BOOT_OK"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# OVMF writes to its "vars" store at runtime (NVRAM emulation) - needs
# a private, writable copy, never the shared template.
OVMF_VARS="$WORKDIR/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"

LOG="$WORKDIR/uefi-boot.log"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$ESP",format=raw,if=virtio \
  -drive file="$SQUASHFS",format=raw,if=virtio,readonly=on \
  -drive file="$VERITY",format=raw,if=virtio,readonly=on \
  -nographic -no-reboot -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT}/" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done

kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

if [ "$code" != "200" ]; then
  echo "UEFI boot test FAILED: no HTTP 200 from HAProxy within ${HTTP_TIMEOUT_SECS}s (last code: '${code}')" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if ! grep -q "BdsDxe: starting Boot" "$LOG"; then
  echo "UEFI boot test FAILED: got HTTP 200, but OVMF's own boot-device log line never appeared - can't confirm this actually went through real UEFI firmware discovery, not some other path" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if ! grep -q "$MARKER" "$LOG"; then
  echo "UEFI boot test FAILED: got HTTP 200 but $MARKER never appeared on the console - investigate" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
echo "UEFI boot test OK: real OVMF firmware discovered and booted the UKI (no -kernel/-append), dm-verity verified root, HAProxy answered HTTP 200"
