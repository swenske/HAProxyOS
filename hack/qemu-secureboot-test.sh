#!/usr/bin/env bash
# Proves Secure Boot signing actually works, both directions - not just
# that `ukify --secureboot-private-key/--secureboot-certificate`
# produces *a* signature, but that real UEFI firmware, with Secure Boot
# genuinely enabled and only our own test key enrolled (--no-microsoft
# is deliberately not even needed here - see image/secureboot/
# enroll-vars.sh: only our cert ever goes into db at all), enforces it:
#
#   1. a UKI signed with the project's (test) key must boot.
#   2. an *unsigned* UKI, on the exact same enrolled vars, must be
#      refused by firmware itself ("Access Denied" in OVMF's own BDS
#      log) - never even reaching the kernel, let alone HTTP.
#
# Needs the secboot-capable OVMF firmware variant (OVMF_CODE_4M.secboot.fd,
# not the plain OVMF_CODE_4M.fd every other boot test here uses) *and*
# `-machine q35,smm=on -global driver=cfi.pflash01,property=secure,value=on`
# - the plain i440fx machine type this project's other tests use
# produces zero console output at all with the secboot firmware binary
# (confirmed empirically: identical command, only the machine type
# changed, before it worked at all).
#
# Usage: hack/qemu-secureboot-test.sh <bzImage> <rootfs-dir>
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <rootfs-dir>}"
ROOTFS_DIR="${2:?usage: $0 <bzImage> <rootfs-dir>}"
BOOT_TIMEOUT_SECS="${QEMU_SECUREBOOT_TIMEOUT:-30}"
MARKER="HAPROXYOS_INIT_BOOT_OK"

OVMF_CODE="${OVMF_CODE_SECBOOT:-/usr/share/OVMF/OVMF_CODE_4M.secboot.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "Secure Boot-capable OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE_SECBOOT to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

"$SELF_DIR/../image/secureboot/gen-test-key.sh" "$WORKDIR"
"$SELF_DIR/../image/secureboot/enroll-vars.sh" "$WORKDIR/vars.fd" "$WORKDIR/cert.pem" "$OVMF_VARS_TEMPLATE"

# Data/hash devices here are /dev/vdb/vdc (ESP takes vda), same
# reasoning as hack/qemu-uefi-boot-test.sh.
"$SELF_DIR/../image/uki/assemble.sh" "$WORKDIR/signed.efi" "$KERNEL" "$ROOTFS_DIR" /dev/vdb /dev/vdc "$WORKDIR/key.pem" "$WORKDIR/cert.pem"
"$SELF_DIR/../image/uki/assemble.sh" "$WORKDIR/unsigned.efi" "$KERNEL" "$ROOTFS_DIR" /dev/vdb /dev/vdc

"$SELF_DIR/../image/uki/esp-image.sh" "$WORKDIR/esp-signed.img" "$WORKDIR/signed.efi" 64
"$SELF_DIR/../image/uki/esp-image.sh" "$WORKDIR/esp-unsigned.img" "$WORKDIR/unsigned.efi" 64

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"

# Boots $1 (an ESP image) under Secure Boot with $WORKDIR/vars.fd and
# writes the console log to $2.
boot() {
  local esp="$1" log="$2"
  timeout "${BOOT_TIMEOUT_SECS}" qemu-system-x86_64 \
    -machine q35,smm=on \
    -global driver=cfi.pflash01,property=secure,value=on \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/vars.fd" \
    -drive file="$esp",format=raw,if=virtio \
    -drive file="$SQUASHFS",format=raw,if=virtio,readonly=on \
    -drive file="$VERITY",format=raw,if=virtio,readonly=on \
    -nographic -no-reboot -display none -m 512M \
    -serial file:"$log" \
    >/dev/null 2>&1 || true
}

SIGNED_LOG="$WORKDIR/signed.log"
boot "$WORKDIR/esp-signed.img" "$SIGNED_LOG"
if ! grep -q "$MARKER" "$SIGNED_LOG"; then
  echo "Secure Boot test FAILED: a UKI signed with the enrolled key was refused (or never booted) - $MARKER never appeared" >&2
  echo "--- console output ---" >&2
  cat "$SIGNED_LOG" >&2
  exit 1
fi
echo "Signed UKI OK: booted successfully under Secure Boot with the matching key enrolled"

UNSIGNED_LOG="$WORKDIR/unsigned.log"
boot "$WORKDIR/esp-unsigned.img" "$UNSIGNED_LOG"
if grep -q "$MARKER" "$UNSIGNED_LOG"; then
  echo "Secure Boot test FAILED: an UNSIGNED UKI booted successfully - Secure Boot enforcement isn't actually working" >&2
  echo "--- console output ---" >&2
  cat "$UNSIGNED_LOG" >&2
  exit 1
fi
if ! grep -qi "Access Denied" "$UNSIGNED_LOG"; then
  echo "Secure Boot test FAILED: the unsigned UKI didn't boot, but no 'Access Denied' message explains why - can't tell this apart from an unrelated failure" >&2
  echo "--- console output ---" >&2
  cat "$UNSIGNED_LOG" >&2
  exit 1
fi
echo "Unsigned UKI OK: refused by firmware (Access Denied) - Secure Boot enforcement confirmed"
echo "Secure Boot test OK: only correctly signed images boot"
