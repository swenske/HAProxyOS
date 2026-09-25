#!/usr/bin/env bash
# Rewrites ONLY the ESP partition of an already-assembled disk image
# (image/disk/assemble.sh) so its Unified Kernel Image boots the given
# A/B slot - BOOT-A-DATA/BOOT-A-HASH (partitions 2/3) or
# BOOT-B-DATA/BOOT-B-HASH (partitions 4/5). Neither slot's own content
# nor STATE (partition 6) is touched at all - this is deliberately the
# minimal, narrow operation a real LifecycleService.Upgrade/Rollback
# will eventually need at the image level: "make the other slot the one
# that boots" without disturbing anything else, most importantly
# whatever's on STATE (PKI, applied config - see rootfs/init/main.go's
# mountState). image/disk/assemble.sh calls this itself for the initial
# ESP write, so there is exactly one place that knows how to build an
# ESP for this disk shape.
#
# Also stages BOTH slots' UKIs on the ESP, at fixed paths
# (\JANUS\UKI-A.EFI, \JANUS\UKI-B.EFI) alongside the active
# one (\EFI\BOOT\BOOTX64.EFI) - not because anything boots them
# directly (only the UEFI-spec fallback path does), but so a running
# node's own LifecycleService.Rollback (internal/api/lifecycle.go) can
# switch the active slot with a plain file copy over
# \EFI\BOOT\BOOTX64.EFI, mounting the ESP itself (CONFIG_VFAT_FS) - the
# target OS has no package manager, so it can never shell out to
# `ukify` the way this script does at build/install time.
#
# Requires ukify (systemd-ukify), mtools/dosfstools, and sgdisk (gdisk,
# to find the ESP partition's own offset/size) - none of it needs root,
# see image/disk/assemble.sh's own note on why.
#
# Usage: image/disk/activate-slot.sh <disk-img> <bzImage> <rootfs-dir> <A|B>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk-img> <bzImage> <rootfs-dir> <A|B>}"
KERNEL="${2:?usage: $0 <disk-img> <bzImage> <rootfs-dir> <A|B>}"
ROOTFS_DIR="${3:?usage: $0 <disk-img> <bzImage> <rootfs-dir> <A|B>}"
ACTIVE_SLOT="${4:?usage: $0 <disk-img> <bzImage> <rootfs-dir> <A|B>}"

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

case "$ACTIVE_SLOT" in
  A|B) ;;
  *) echo "invalid slot '$ACTIVE_SLOT' - must be A or B" >&2; exit 1 ;;
esac

ESP_START_SECTOR="$(sgdisk -i 1 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
ESP_SIZE_SECTORS="$(sgdisk -i 1 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
ESP_MB=$(( ESP_SIZE_SECTORS * 512 / 1024 / 1024 ))

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

UKI_A="$WORKDIR/uki-a.efi"
UKI_B="$WORKDIR/uki-b.efi"
"$SELF_DIR/../uki/assemble.sh" "$UKI_A" "$KERNEL" "$ROOTFS_DIR" /dev/vda2 /dev/vda3
"$SELF_DIR/../uki/assemble.sh" "$UKI_B" "$KERNEL" "$ROOTFS_DIR" /dev/vda4 /dev/vda5

ACTIVE_UKI="$UKI_A"
[ "$ACTIVE_SLOT" = "B" ] && ACTIVE_UKI="$UKI_B"

ESP_IMG="$WORKDIR/esp.img"
"$SELF_DIR/../uki/esp-image.sh" "$ESP_IMG" "$ACTIVE_UKI" "$ESP_MB"
mmd -i "$ESP_IMG" ::/JANUS
mcopy -i "$ESP_IMG" "$UKI_A" ::/JANUS/UKI-A.EFI
mcopy -i "$ESP_IMG" "$UKI_B" ::/JANUS/UKI-B.EFI

dd if="$ESP_IMG" of="$DISK" bs=512 seek="$ESP_START_SECTOR" conv=notrunc status=none

echo "Slot $ACTIVE_SLOT is now active on $DISK's ESP (both UKI-A.EFI and UKI-B.EFI staged under \\JANUS\\)"
