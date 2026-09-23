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
# The dm-verity table syntax/field order (see hack/dm-verity-cmdline.sh,
# shared with the other boot tests and image/uki/assemble.sh - in
# particular hash_start_block=1: the hash tree always starts one
# hash-block after veritysetup's own superblock, which occupies exactly
# hash_block_size bytes at offset 0 since assemble.sh never passes
# --hash-offset) and the need for CONFIG_CRYPTO_SHA256 (not just
# CONFIG_CRYPTO_LIB_SHA256 - dm-verity resolves "sha256" through the
# crypto API by name, not the raw lib helper) were both confirmed by an
# actual boot failing first, not assumed from documentation alone.
#
# Runs two boots:
#   1. the real, untampered image, with virtio-net + DHCP like Phase 2's
#      network test - must not just print the boot marker but actually
#      have HAProxy answer real HTTP, proving rootfs/init's ephemeral
#      tmpfs overlay (see rootfs/init/main.go's mountEphemeral) makes
#      the verified-read-only root genuinely bootable end to end:
#      haproxyosd's PKI bootstrap and HAProxy's own startup both need to
#      write to paths that live under /etc and /run.
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
BOOT_TIMEOUT_SECS="${QEMU_VERITY_BOOT_TIMEOUT:-30}"
HTTP_TIMEOUT_SECS="${QEMU_VERITY_HTTP_TIMEOUT:-30}"
HOST_PORT="${QEMU_VERITY_TEST_PORT:-18082}"
MARKER="HAPROXYOS_INIT_BOOT_OK"

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"

dm_table() {
  "$(dirname "$0")/dm-verity-cmdline.sh" "$ROOTFS_DIR" /dev/vda /dev/vdb
}

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# --- 1. real image, over the network, must actually serve HTTP ---
GOOD_LOG="$WORKDIR/good.log"
qemu-system-x86_64 \
  -kernel "$KERNEL" \
  -append "console=ttyS0 panic=-1 dm-mod.create=\"$(dm_table)\" root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp" \
  -nographic -no-reboot -display none -m 256M \
  -drive file="$SQUASHFS",format=raw,if=virtio,readonly=on \
  -drive file="$VERITY",format=raw,if=virtio,readonly=on \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$GOOD_LOG" \
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
  echo "Verity boot FAILED: no HTTP 200 from HAProxy within ${HTTP_TIMEOUT_SECS}s (last code: '${code}')" >&2
  echo "--- console output ---" >&2
  cat "$GOOD_LOG" >&2
  exit 1
fi
if ! grep -q "$MARKER" "$GOOD_LOG"; then
  echo "Verity boot FAILED: got HTTP 200 but $MARKER never appeared on the console - investigate" >&2
  echo "--- console output ---" >&2
  cat "$GOOD_LOG" >&2
  exit 1
fi
echo "Verity boot OK: $MARKER printed and HAProxy answered HTTP 200, from the dm-verity-verified root"

# --- 2. tampered image, must never even reach userspace ---
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
timeout "${BOOT_TIMEOUT_SECS}" qemu-system-x86_64 \
  -kernel "$KERNEL" \
  -append "console=ttyS0 panic=-1 dm-mod.create=\"$(dm_table)\" root=/dev/dm-0 rootfstype=squashfs ro" \
  -nographic -no-reboot -m 256M \
  -drive file="$TAMPERED",format=raw,if=virtio,readonly=on \
  -drive file="$VERITY",format=raw,if=virtio,readonly=on \
  -serial mon:stdio \
  >"$TAMPER_LOG" 2>&1 || true

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
