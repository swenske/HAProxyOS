#!/usr/bin/env bash
# Assembles a single, real GPT-partitioned disk image with two
# independently bootable A/B slots - BOOT-A-DATA/BOOT-A-HASH and
# BOOT-B-DATA/BOOT-B-HASH, each a squashfs data + dm-verity hash tree
# pair, same content rootfs/assemble.sh already produces - plus the
# persistent STATE partition (rootfs/state-image.sh). This is the real,
# single-disk shape a deployed node would actually have, as opposed to
# this project's earlier QEMU test harnesses
# (hack/qemu-verity-boot-test.sh, hack/qemu-state-persist-test.sh),
# which each attach the same build artifacts as separate virtio-blk
# drives rather than partitions of one disk - those keep working
# unchanged and still cover what they always covered (dm-verity tamper
# detection, STATE persistence); this script and
# hack/qemu-ab-boot-test.sh cover something they don't: that the real
# on-disk layout (one disk, GPT, two slots) actually works, and that
# both slots boot independently.
#
# Both slots get the SAME content at build time - there's no
# LifecycleService.Upgrade yet to install something different into the
# inactive slot (see docs/architecture.md's Phase 3 notes), so this
# only proves the layout/dm-verity-via-partition-device mechanics, not
# a real upgrade workflow.
#
# rootfs/init/main.go's mountState still expects the STATE partition as
# a separate virtio-blk drive (/dev/vdc) - it does NOT yet know how to
# find the STATE partition (or which slot it booted from) on a real
# single-disk system. Partition 5 here exists to prove the on-disk
# layout is real, not because anything reads it there yet - see
# docs/architecture.md for why that's a distinct, harder problem
# (no udev, so no /dev/disk/by-partlabel/* to just look up).
#
# Requires sgdisk (gdisk) - doesn't need root: writes go straight to
# computed byte offsets in the image file, not through a mounted
# filesystem or loop device (unlike a real installer, which writes to
# an actual disk - this script only ever touches a plain file). See
# rootfs/state-image.sh's own note on why loop devices are avoided
# deliberately, not just incidentally, in this project's build tooling.
#
# Usage: image/disk/assemble.sh <out-file> <rootfs-dir> <state-image>
# <rootfs-dir> must contain rootfs.squashfs and rootfs.verity (see
# rootfs/assemble.sh). <state-image> from rootfs/state-image.sh.
set -euo pipefail

# Same PATH gap as veritysetup/mkfs.ext4/debugfs before it - sgdisk
# (gdisk) installs to /usr/sbin too.
export PATH="$PATH:/usr/sbin:/sbin"

OUT="${1:?usage: $0 <out-file> <rootfs-dir> <state-image>}"
ROOTFS_DIR="${2:?usage: $0 <out-file> <rootfs-dir> <state-image>}"
STATE_IMAGE="${3:?usage: $0 <out-file> <rootfs-dir> <state-image>}"

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"

# Fixed, over-provisioned partition sizes, matching a real A/B system:
# the partition table is laid out once and doesn't grow with the image
# - rootfs.squashfs (~8MiB today) and rootfs.verity (~72KiB today) just
# need to fit inside them, with headroom for the rootfs to grow later
# without needing a new partition table.
DATA_MB=64
HASH_MB=4
STATE_MB=16

size_of() { stat -c%s "$1"; }
[ "$(size_of "$SQUASHFS")" -le $((DATA_MB * 1024 * 1024)) ] || { echo "rootfs.squashfs exceeds the ${DATA_MB}MiB BOOT-*-DATA partition size" >&2; exit 1; }
[ "$(size_of "$VERITY")" -le $((HASH_MB * 1024 * 1024)) ] || { echo "rootfs.verity exceeds the ${HASH_MB}MiB BOOT-*-HASH partition size" >&2; exit 1; }
[ "$(size_of "$STATE_IMAGE")" -le $((STATE_MB * 1024 * 1024)) ] || { echo "state.img exceeds the ${STATE_MB}MiB STATE partition size" >&2; exit 1; }

# +2MiB slack on top of the five partitions' own sizes, for GPT's
# primary/backup headers and sgdisk's own partition alignment - sgdisk
# will refuse to lay out a disk that's merely "exactly big enough".
TOTAL_MB=$(( DATA_MB + HASH_MB + DATA_MB + HASH_MB + STATE_MB + 2 ))
truncate -s "${TOTAL_MB}M" "$OUT"

sgdisk --zap-all "$OUT" >/dev/null
sgdisk \
  -n 1:0:+"${DATA_MB}"M -t 1:8300 -c 1:BOOT-A-DATA \
  -n 2:0:+"${HASH_MB}"M -t 2:8300 -c 2:BOOT-A-HASH \
  -n 3:0:+"${DATA_MB}"M -t 3:8300 -c 3:BOOT-B-DATA \
  -n 4:0:+"${HASH_MB}"M -t 4:8300 -c 4:BOOT-B-HASH \
  -n 5:0:0              -t 5:8300 -c 5:STATE \
  "$OUT" >/dev/null

# Writes $2 at partition $1's own starting byte offset - sgdisk -i
# prints the exact sector it actually landed on, rather than this
# script re-deriving it by hand from the sizes above (which would
# silently drift if sgdisk's own alignment rules ever changed).
write_part() {
  local part="$1" src="$2" start_sector
  start_sector="$(sgdisk -i "$part" "$OUT" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
  dd if="$src" of="$OUT" bs=512 seek="$start_sector" conv=notrunc status=none
}

write_part 1 "$SQUASHFS"
write_part 2 "$VERITY"
write_part 3 "$SQUASHFS"
write_part 4 "$VERITY"
write_part 5 "$STATE_IMAGE"

echo "Wrote $OUT (${TOTAL_MB}MiB GPT disk: BOOT-A-DATA/HASH, BOOT-B-DATA/HASH, STATE)"
sgdisk -p "$OUT"
