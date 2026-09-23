#!/usr/bin/env bash
# Proves rootfs/state-image.sh's persistent STATE partition actually
# survives a reboot - the gap explicitly left open when the ephemeral
# tmpfs overlay landed (see rootfs/init/main.go's mountEphemeral/
# mountState and docs/architecture.md's Phase 3 notes). Boots the same
# dm-verity-verified image twice against the SAME state.img file, this
# one attached as a third, *writable* virtio-blk drive (unlike the two
# read-only root drives):
#
#   1. first boot - PKI hasn't been bootstrapped yet, haproxyosd must
#      log "pki: first boot - generated a new CA" and write it to
#      /etc/haproxyos/pki, which rootfs/init/main.go's mountState mounts
#      from the persistent partition, not the ephemeral tmpfs. haproxyosd
#      calls syscall.Sync() right after writing those files (see
#      cmd/haproxyosd/main.go), so this isn't relying on QEMU's
#      shutdown-time cache flush to make them durable.
#   2. second boot, same state.img (now holding boot 1's CA/certs) -
#      haproxyosd must NOT log that message again: internal/pki.
#      LoadOrBootstrap finds an existing ca.crt and loads it instead.
#      If this fired again, /etc/haproxyos/pki would still be ephemeral
#      in practice regardless of whether the mount itself "succeeded".
#
# Usage: hack/qemu-state-persist-test.sh <bzImage> <rootfs-dir> <state-image>
# <rootfs-dir> must contain rootfs.squashfs, rootfs.verity,
# rootfs.roothash and rootfs.verity.info (see rootfs/assemble.sh).
# <state-image> is a writable ext4 image (see rootfs/state-image.sh) -
# this script mutates it in place across the two boots.
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <rootfs-dir> <state-image>}"
ROOTFS_DIR="${2:?usage: $0 <bzImage> <rootfs-dir> <state-image>}"
STATE_IMAGE="${3:?usage: $0 <bzImage> <rootfs-dir> <state-image>}"
HTTP_TIMEOUT_SECS="${QEMU_STATE_HTTP_TIMEOUT:-30}"
HOST_PORT="${QEMU_STATE_TEST_PORT:-18083}"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

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
  echo "vroot,,,ro,0 $SECTORS verity 1 /dev/vda /dev/vdb $DATA_BLOCK_SIZE $HASH_BLOCK_SIZE $DATA_BLOCKS 1 sha256 $ROOTHASH $SALT"
}

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# Boots once against $STATE_IMAGE (mutated in place) and polls for HTTP
# 200, writing the console log to $1. Returns non-zero if HAProxy never
# answered within the timeout.
boot_and_wait_http() {
  local log="$1"
  qemu-system-x86_64 \
    -kernel "$KERNEL" \
    -append "console=ttyS0 panic=-1 dm-mod.create=\"$(dm_table)\" root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp" \
    -nographic -no-reboot -display none -m 256M \
    -drive file="$SQUASHFS",format=raw,if=virtio,readonly=on \
    -drive file="$VERITY",format=raw,if=virtio,readonly=on \
    -drive file="$STATE_IMAGE",format=raw,if=virtio \
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

BOOT1_LOG="$WORKDIR/boot1.log"
if ! boot_and_wait_http "$BOOT1_LOG"; then
  echo "State persist test FAILED: no HTTP 200 on first boot within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$BOOT1_LOG" >&2
  exit 1
fi
if ! grep -q "$FIRST_BOOT_MSG" "$BOOT1_LOG"; then
  echo "State persist test FAILED: first boot didn't log '$FIRST_BOOT_MSG' - expected a fresh bootstrap against a blank state partition" >&2
  echo "--- console output ---" >&2
  cat "$BOOT1_LOG" >&2
  exit 1
fi
echo "First boot OK: bootstrapped a new CA onto the persistent STATE partition"

BOOT2_LOG="$WORKDIR/boot2.log"
if ! boot_and_wait_http "$BOOT2_LOG"; then
  echo "State persist test FAILED: no HTTP 200 on second boot within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$BOOT2_LOG" >&2
  exit 1
fi
if grep -q "$FIRST_BOOT_MSG" "$BOOT2_LOG"; then
  echo "State persist test FAILED: second boot logged '$FIRST_BOOT_MSG' again - the CA didn't persist, /etc/haproxyos/pki is still effectively ephemeral" >&2
  echo "--- console output ---" >&2
  cat "$BOOT2_LOG" >&2
  exit 1
fi
echo "Second boot OK: loaded the existing CA from the persistent STATE partition - no re-bootstrap"
