#!/usr/bin/env bash
# Prints the dm-mod.create= *value* (the part between the quotes - see
# Documentation/admin-guide/device-mapper/dm-init.rst in the kernel
# source) for a single dm-verity mapping named "vroot" over the given
# data/hash devices, derived from rootfs/assemble.sh's
# rootfs.verity.info + rootfs.roothash. Shared by every script that
# needs to boot from a dm-verity root (hack/qemu-verity-boot-test.sh,
# hack/qemu-ab-boot-test.sh, hack/qemu-state-persist-test.sh,
# image/uki/assemble.sh) so the table format/field order - confirmed
# empirically against real boots, not assumed from docs alone, see
# those scripts' own comments - lives in exactly one place.
#
# Usage: hack/dm-verity-cmdline.sh <rootfs-dir> <data-device> <hash-device>
set -euo pipefail

ROOTFS_DIR="${1:?usage: $0 <rootfs-dir> <data-device> <hash-device>}"
DATA_DEV="${2:?usage: $0 <rootfs-dir> <data-device> <hash-device>}"
HASH_DEV="${3:?usage: $0 <rootfs-dir> <data-device> <hash-device>}"

INFO="$ROOTFS_DIR/rootfs.verity.info"
ROOTHASH="$(cat "$ROOTFS_DIR/rootfs.roothash")"
SALT="$(grep '^Salt:' "$INFO" | awk '{print $2}')"
DATA_BLOCKS="$(grep '^Data blocks:' "$INFO" | awk '{print $3}')"
DATA_BLOCK_SIZE="$(grep '^Data block size:' "$INFO" | awk '{print $4}')"
HASH_BLOCK_SIZE="$(grep '^Hash block size:' "$INFO" | awk '{print $4}')"
SECTORS=$(( DATA_BLOCKS * DATA_BLOCK_SIZE / 512 ))

echo "vroot,,,ro,0 $SECTORS verity 1 $DATA_DEV $HASH_DEV $DATA_BLOCK_SIZE $HASH_BLOCK_SIZE $DATA_BLOCKS 1 sha256 $ROOTHASH $SALT"
