#!/usr/bin/env bash
# Builds a FAT32 EFI System Partition (ESP) image containing the given
# Unified Kernel Image at the UEFI-spec-mandated removable-media fallback
# path (\EFI\BOOT\BOOTX64.EFI) - the one firmware auto-boots with no
# NVRAM boot entry configured at all, which is all this project needs
# (no boot menu, matching the "no shell, no unnecessary components"
# design). Built with mtools (mformat/mmd/mcopy) directly against the
# image file - no mount, no loop device, same reasoning as
# rootfs/state-image.sh and image/disk/assemble.sh (loop devices aren't
# available on janus-runner01).
#
# Usage: image/uki/esp-image.sh <out-file> <uki.efi> <size-mb>
set -euo pipefail

# mkfs.vfat (dosfstools) installs to /usr/sbin, same PATH gap already
# hit for veritysetup/mkfs.ext4/debugfs/sgdisk.
export PATH="$PATH:/usr/sbin:/sbin"

OUT="${1:?usage: $0 <out-file> <uki.efi> <size-mb>}"
UKI="${2:?usage: $0 <out-file> <uki.efi> <size-mb>}"
SIZE_MB="${3:?usage: $0 <out-file> <uki.efi> <size-mb>}"

truncate -s "${SIZE_MB}M" "$OUT"
mkfs.vfat -F 32 "$OUT" >/dev/null
mmd -i "$OUT" ::/EFI
mmd -i "$OUT" ::/EFI/BOOT
mcopy -i "$OUT" "$UKI" ::/EFI/BOOT/BOOTX64.EFI

echo "Wrote $OUT (${SIZE_MB}MiB FAT32 ESP, \\EFI\\BOOT\\BOOTX64.EFI = $UKI)"
