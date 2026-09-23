#!/usr/bin/env bash
# Assembles a single, real GPT-partitioned disk image - the actual,
# complete shape a deployed node would have: an EFI System Partition
# (ESP) with the active slot's Unified Kernel Image at
# \EFI\BOOT\BOOTX64.EFI (image/uki/assemble.sh + image/uki/esp-image.sh
# - no boot manager, matching this project's "no unnecessary
# components" design, see hack/qemu-uefi-boot-test.sh), two
# independently bootable A/B slots (BOOT-A-DATA/BOOT-A-HASH and
# BOOT-B-DATA/BOOT-B-HASH, each a squashfs data + dm-verity hash tree
# pair, same content rootfs/assemble.sh already produces), and the
# persistent STATE partition (rootfs/state-image.sh).
#
# This project's earlier QEMU test harnesses
# (hack/qemu-verity-boot-test.sh, hack/qemu-state-persist-test.sh) each
# attach the same build artifacts as separate virtio-blk drives rather
# than partitions of one disk - those keep working unchanged and still
# cover what they always covered (dm-verity tamper detection, STATE
# persistence). This script, hack/qemu-ab-boot-test.sh and
# hack/qemu-uefi-boot-test.sh cover something they don't: that the
# real, single-disk, UEFI-bootable layout actually works.
#
# Both A/B slots get the SAME rootfs content at build time - there's no
# LifecycleService.Upgrade yet to install something different into the
# inactive slot (see docs/architecture.md's Phase 3 notes). "Active"
# here only controls which slot's device paths get baked into the ESP's
# UKI cmdline - see image/disk/activate-slot.sh for switching that on
# an already-assembled disk, in place, without touching STATE or either
# slot's content, which is the operation a real
# LifecycleService.Upgrade/Rollback will eventually drive.
#
# Requires sgdisk (gdisk), ukify (systemd-ukify), mtools/dosfstools -
# none of it needs root: writes go straight to computed byte offsets in
# the image file, not through a mounted filesystem or loop device
# (unlike a real installer, which writes to an actual disk - this
# script only ever touches a plain file). See rootfs/state-image.sh's
# own note on why loop devices are avoided deliberately, not just
# incidentally, in this project's build tooling.
#
# Device paths baked into the UKI's cmdline assume this disk attaches
# as /dev/vda (true for every QEMU test in this project, which only
# ever attaches one virtio-blk drive as root) - a real installer
# targeting arbitrary hardware would need to resolve this dynamically,
# not hardcode it; out of scope here.
#
# Usage: image/disk/assemble.sh <out-file> <bzImage> <rootfs-dir> <state-image> [active-slot]
# <rootfs-dir> must contain rootfs.squashfs and rootfs.verity (see
# rootfs/assemble.sh). <state-image> from rootfs/state-image.sh.
# [active-slot] is "A" or "B", defaulting to "A".
set -euo pipefail

# Same PATH gap as veritysetup/mkfs.ext4/debugfs/mkfs.vfat before it.
export PATH="$PATH:/usr/sbin:/sbin"

OUT="${1:?usage: $0 <out-file> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
KERNEL="${2:?usage: $0 <out-file> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
ROOTFS_DIR="${3:?usage: $0 <out-file> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
STATE_IMAGE="${4:?usage: $0 <out-file> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
ACTIVE_SLOT="${5:-A}"

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

# Fixed, over-provisioned partition sizes, matching a real A/B system:
# the partition table is laid out once and doesn't grow with the image
# - rootfs.squashfs (~8MiB today) and rootfs.verity (~72KiB today) just
# need to fit inside them, with headroom for the rootfs to grow later
# without needing a new partition table. ESP_MB (64) comfortably fits
# one UKI (~4.5MiB today).
ESP_MB=64
DATA_MB=64
HASH_MB=4
STATE_MB=16

size_of() { stat -c%s "$1"; }
[ "$(size_of "$SQUASHFS")" -le $((DATA_MB * 1024 * 1024)) ] || { echo "rootfs.squashfs exceeds the ${DATA_MB}MiB BOOT-*-DATA partition size" >&2; exit 1; }
[ "$(size_of "$VERITY")" -le $((HASH_MB * 1024 * 1024)) ] || { echo "rootfs.verity exceeds the ${HASH_MB}MiB BOOT-*-HASH partition size" >&2; exit 1; }
[ "$(size_of "$STATE_IMAGE")" -le $((STATE_MB * 1024 * 1024)) ] || { echo "state.img exceeds the ${STATE_MB}MiB STATE partition size" >&2; exit 1; }

# +2MiB slack on top of the six partitions' own sizes, for GPT's
# primary/backup headers and sgdisk's own partition alignment - sgdisk
# will refuse to lay out a disk that's merely "exactly big enough".
TOTAL_MB=$(( ESP_MB + DATA_MB + HASH_MB + DATA_MB + HASH_MB + STATE_MB + 2 ))
truncate -s "${TOTAL_MB}M" "$OUT"

sgdisk --zap-all "$OUT" >/dev/null
sgdisk \
  -n 1:0:+"${ESP_MB}"M  -t 1:ef00 -c 1:ESP \
  -n 2:0:+"${DATA_MB}"M -t 2:8300 -c 2:BOOT-A-DATA \
  -n 3:0:+"${HASH_MB}"M -t 3:8300 -c 3:BOOT-A-HASH \
  -n 4:0:+"${DATA_MB}"M -t 4:8300 -c 4:BOOT-B-DATA \
  -n 5:0:+"${HASH_MB}"M -t 5:8300 -c 5:BOOT-B-HASH \
  -n 6:0:0              -t 6:8300 -c 6:STATE \
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

"$SELF_DIR/activate-slot.sh" "$OUT" "$KERNEL" "$ROOTFS_DIR" "$ACTIVE_SLOT"

write_part 2 "$SQUASHFS"
write_part 3 "$VERITY"
write_part 4 "$SQUASHFS"
write_part 5 "$VERITY"
write_part 6 "$STATE_IMAGE"

echo "Wrote $OUT (${TOTAL_MB}MiB GPT disk: ESP, BOOT-A-DATA/HASH, BOOT-B-DATA/HASH, STATE; active slot $ACTIVE_SLOT)"
sgdisk -p "$OUT"
