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
# Usage: image/uki/assemble.sh <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]
# <rootfs-dir> must contain rootfs.verity.info/rootfs.roothash (see
# rootfs/assemble.sh and hack/dm-verity-cmdline.sh). <data-device>/
# <hash-device> are the two virtio-blk devices root will actually be
# attached as at boot (e.g. /dev/vdb /dev/vdc, if an ESP is vda) -
# baked into the UKI's cmdline permanently, since there's no way to
# override it after the fact without reassembling.
#
# [signing-key]/[signing-cert] are optional - given both, the UKI is
# Secure Boot-signed (`ukify` shells out to `sbsign`) with them; given
# neither (the default, used by every caller except
# hack/qemu-secureboot-test.sh), the UKI is unsigned, exactly as
# before - Secure Boot enforcement is opt-in at the firmware/vars level
# (see image/secureboot/), not something every other boot test in this
# project needs to care about.
set -euo pipefail

OUT="${1:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
KERNEL="${2:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
ROOTFS_DIR="${3:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
DATA_DEV="${4:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
HASH_DEV="${5:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
SIGNING_KEY="${6:-}"
SIGNING_CERT="${7:-}"

DM_TABLE="$(dirname "$0")/../../hack/dm-verity-cmdline.sh"
CMDLINE_FILE="$(mktemp)"
trap 'rm -f "$CMDLINE_FILE"' EXIT
{
  # enforcing=1: Phase 4 cont'd's real SELinux policy (selinux/) already
  # proved a clean, zero-denial boot under enforcing mode (see
  # hack/qemu-selinux-test.sh) - this is what actually makes that the
  # shipped default rather than just a fact proven about a test boot.
  # kernel/configs/haproxyos_defconfig's own SECURITY_SELINUX_DEVELOP=y
  # deliberately stays on regardless (so /sys/fs/selinux/enforce can
  # still be toggled interactively for debugging, and the kernel's own
  # default without this cmdline override would still be the safer
  # permissive one) - this UKI-baked cmdline is what makes enforcing the
  # real, permanent default in practice, since there's no boot menu to
  # add it from later.
  printf 'console=ttyS0 panic=-1 dm-mod.create="%s" root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp enforcing=1' \
    "$("$DM_TABLE" "$ROOTFS_DIR" "$DATA_DEV" "$HASH_DEV")"
} > "$CMDLINE_FILE"

mkdir -p "$(dirname "$OUT")"
UKIFY_ARGS=(
  build
  --linux="$KERNEL"
  --cmdline="@$CMDLINE_FILE"
  --os-release="$(printf 'NAME=HAProxyOS\nPRETTY_NAME=HAProxyOS\nID=haproxyos\n')"
  -o "$OUT"
)
if [ -n "$SIGNING_KEY" ] && [ -n "$SIGNING_CERT" ]; then
  UKIFY_ARGS+=(--secureboot-private-key="$SIGNING_KEY" --secureboot-certificate="$SIGNING_CERT")
fi
ukify "${UKIFY_ARGS[@]}"

echo "Wrote $OUT"
