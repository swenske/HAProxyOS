#!/usr/bin/env bash
# Assembles a real Unified Kernel Image (UKI) - the kernel plus its
# exact boot command line, bundled into one PE/COFF executable UEFI
# firmware can load and run directly, no separate bootloader, no
# initramfs, no interactive EFI shell needed to pass arguments. Uses
# `ukify` (systemd-ukify) rather than hand-assembling one with objcopy:
# the vanilla kernel's own EFI stub only ever reads its command line
# from the EFI LoadOptions the firmware passes when launching an image
# interactively (see Documentation/admin-guide/efi-stub.rst in the
# kernel source and drivers/firmware/efi/libstub/efi-stub-helper.c's
# efi_convert_cmdline) - it does NOT look for a `.cmdline` PE section on
# its own. `ukify`'s stub (systemd-stub, linuxx64.efi.stub) does, and
# then chain-loads into the kernel's own embedded EFI stub via the EFI
# handover protocol (CONFIG_EFI_HANDOVER_PROTOCOL) - which is exactly
# why the kernel needs CONFIG_EFI_STUB itself too, not just systemd's
# stub: the code receiving that handover call lives in the kernel.
#
# Booting this with no drive attached at the STATE partition device
# (see rootfs/init/main.go's mountState) is fine - it falls back to the
# ephemeral tmpfs for PKI/config the same way it always has; this
# script only cares about root=, not STATE.
#
# Usage: image/uki/assemble.sh <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device>
# <rootfs-dir> must contain rootfs.verity.info/rootfs.roothash (see
# rootfs/assemble.sh and hack/dm-verity-cmdline.sh). <data-device>/
# <hash-device> are the two virtio-blk devices root will actually be
# attached as at boot (e.g. /dev/vdb /dev/vdc, if an ESP is vda) -
# baked into the UKI's cmdline permanently, since there's no way to
# override it after the fact without reassembling.
set -euo pipefail

OUT="${1:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device>}"
KERNEL="${2:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device>}"
ROOTFS_DIR="${3:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device>}"
DATA_DEV="${4:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device>}"
HASH_DEV="${5:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device>}"

DM_TABLE="$(dirname "$0")/../../hack/dm-verity-cmdline.sh"
CMDLINE_FILE="$(mktemp)"
trap 'rm -f "$CMDLINE_FILE"' EXIT
{
  printf 'console=ttyS0 panic=-1 dm-mod.create="%s" root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp' \
    "$("$DM_TABLE" "$ROOTFS_DIR" "$DATA_DEV" "$HASH_DEV")"
} > "$CMDLINE_FILE"

mkdir -p "$(dirname "$OUT")"
ukify build \
  --linux="$KERNEL" \
  --cmdline="@$CMDLINE_FILE" \
  --os-release="$(printf 'NAME=HAProxyOS\nPRETTY_NAME=HAProxyOS\nID=haproxyos\n')" \
  -o "$OUT"

echo "Wrote $OUT"
