#!/usr/bin/env bash
# Boots a built HAProxyOS kernel directly from rootfs/assemble.sh's
# squashfs+dm-verity image - no initramfs, no userspace verity setup at
# all. The kernel itself assembles /dev/dm-0 from the two virtio-blk
# drives below via the "dm-mod.create=" cmdline parameter (see
# Documentation/admin-guide/device-mapper/dm-init.rst in the kernel
# source - CONFIG_DM_INIT is exactly the feature this exists for: "allow
# mounting rootfs without requiring an initramfs"), verifies it against
# the given root hash, and mounts *that* as the real root before running
# /sbin/init straight out of the verified image (see rootfs/assemble.sh
# for what's in there).
#
# The dm-verity table syntax/field order below (in particular
# hash_start_block=1 - the hash tree always starts one hash-block after
# veritysetup's own superblock, which occupies exactly hash_block_size
# bytes at offset 0 since assemble.sh never passes --hash-offset) and
# the need for CONFIG_CRYPTO_SHA256 (not just CONFIG_CRYPTO_LIB_SHA256 -
# dm-verity resolves "sha256" through the crypto API by name, not the
# raw lib helper) were both confirmed by an actual boot failing first,
# not assumed from documentation alone.
#
# Runs two boots:
#   1. the real, untampered image - must print the boot marker.
#   2. a copy of rootfs.squashfs with one byte flipped - dm-verity must
#      refuse to mount it (no marker, kernel panics trying to mount
#      root), proving the *kernel*, not just `veritysetup verify` on the
#      build host (see image-build.yml's own tamper test), enforces
#      integrity at boot.
#
# Usage: hack/qemu-verity-boot-test.sh <bzImage> <rootfs-dir>
# <rootfs-dir> must contain rootfs.squashfs, rootfs.verity,
# rootfs.roothash and rootfs.verity.info (see rootfs/assemble.sh).
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <rootfs-dir>}"
ROOTFS_DIR="${2:?usage: $0 <bzImage> <rootfs-dir>}"
TIMEOUT_SECS="${QEMU_VERITY_BOOT_TIMEOUT:-30}"
MARKER="HAPROXYOS_INIT_BOOT_OK"

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"
INFO="$ROOTFS_DIR/rootfs.verity.info"
ROOTHASH="$(cat "$ROOTFS_DIR/rootfs.roothash")"
SALT="$(grep '^Salt:' "$INFO" | awk '{print $2}')"
DATA_BLOCKS="$(grep '^Data blocks:' "$INFO" | awk '{print $3}')"
DATA_BLOCK_SIZE="$(grep '^Data block size:' "$INFO" | awk '{print $4}')"
HASH_BLOCK_SIZE="$(grep '^Hash block size:' "$INFO" | awk '{print $4}')"
SECTORS=$(( DATA_BLOCKS * DATA_BLOCK_SIZE / 512 ))

dm_table() {
  # $1 = squashfs image path to boot from (real or tampered)
  echo "vroot,,,ro,0 $SECTORS verity 1 /dev/vda /dev/vdb $DATA_BLOCK_SIZE $HASH_BLOCK_SIZE $DATA_BLOCKS 1 sha256 $ROOTHASH $SALT"
}

boot() {
  # $1 = squashfs image to attach as /dev/vda, $2 = output log path
  local img="$1" log="$2"
  timeout "${TIMEOUT_SECS}" qemu-system-x86_64 \
    -kernel "$KERNEL" \
    -append "console=ttyS0 panic=-1 dm-mod.create=\"$(dm_table)\" root=/dev/dm-0 rootfstype=squashfs ro" \
    -nographic -no-reboot -m 256M \
    -drive file="$img",format=raw,if=virtio,readonly=on \
    -drive file="$VERITY",format=raw,if=virtio,readonly=on \
    -serial mon:stdio \
    >"$log" 2>&1 || true
}

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

GOOD_LOG="$WORKDIR/good.log"
boot "$SQUASHFS" "$GOOD_LOG"
if ! grep -q "$MARKER" "$GOOD_LOG"; then
  echo "Verity boot FAILED: $MARKER not found booting the real image within ${TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$GOOD_LOG" >&2
  exit 1
fi
echo "Verity boot OK: found $MARKER booting the real dm-verity-protected image"

TAMPERED="$WORKDIR/tampered.squashfs"
cp "$SQUASHFS" "$TAMPERED"
# Corrupting the very first byte (squashfs's own superblock) guarantees
# the corrupted block is read immediately when the kernel tries to mount
# root - unlike a random offset deeper in the image, which might land in
# a file (e.g. the haproxy binary) that init only reads *after* printing
# the boot marker, making the corruption invisible to this test (checked
# empirically: corrupting byte 500000 - the offset image-build.yml's
# userspace-level `veritysetup verify` tamper test uses - still let this
# boot print the marker just fine).
printf '\xFF' | dd of="$TAMPERED" bs=1 seek=0 count=1 conv=notrunc status=none

TAMPER_LOG="$WORKDIR/tampered.log"
boot "$TAMPERED" "$TAMPER_LOG"
if grep -q "$MARKER" "$TAMPER_LOG"; then
  echo "Verity boot FAILED: tampered image booted successfully (found $MARKER) - dm-verity did not stop it" >&2
  echo "--- console output ---" >&2
  cat "$TAMPER_LOG" >&2
  exit 1
fi
if ! grep -qi "verity" "$TAMPER_LOG"; then
  echo "Verity boot FAILED: tampered image didn't boot, but no dm-verity message explains why - can't tell this apart from an unrelated failure" >&2
  echo "--- console output ---" >&2
  cat "$TAMPER_LOG" >&2
  exit 1
fi
echo "Tamper test OK: dm-verity blocked the corrupted image at boot (no $MARKER, verity error present)"
