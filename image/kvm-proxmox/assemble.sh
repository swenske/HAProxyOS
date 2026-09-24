#!/usr/bin/env bash
# Wraps image/disk/assemble.sh's real, single-disk GPT image (ESP +
# both A/B slots + STATE - see that script's own header) into qcow2,
# Proxmox's own preferred import/storage format (thin-provisioned,
# snapshot-capable - a plain raw file works too, but qcow2 is what
# `qm importdisk`/the Proxmox storage backends are built around).
#
# Usage: image/kvm-proxmox/assemble.sh <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]
# Same inputs as image/disk/assemble.sh - see its own header for what
# <rootfs-dir>/<state-image> need to contain.
#
# Requires everything image/disk/assemble.sh does (sgdisk, ukify,
# mtools/dosfstools), plus qemu-img.
set -euo pipefail

OUT="${1:?usage: $0 <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
KERNEL="${2:?usage: $0 <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
ROOTFS_DIR="${3:?usage: $0 <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
STATE_IMAGE="${4:?usage: $0 <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
ACTIVE_SLOT="${5:-A}"

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

RAW="$(mktemp)"
trap 'rm -f "$RAW"' EXIT
"$SELF_DIR/../disk/assemble.sh" "$RAW" "$KERNEL" "$ROOTFS_DIR" "$STATE_IMAGE" "$ACTIVE_SLOT"

mkdir -p "$(dirname "$OUT")"
# -c: compress at conversion time (this is a one-shot conversion of an
# already-final image, not an ongoing write path - the VM's own later
# writes to the qcow2 file are unaffected, still normal uncompressed
# cluster writes).
qemu-img convert -O qcow2 -c "$RAW" "$OUT"

echo "Wrote $OUT ($(du -h "$OUT" | cut -f1) qcow2, from a $(du -h "$RAW" | cut -f1) raw GPT disk)"
