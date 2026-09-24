#!/usr/bin/env bash
# Proves Phase 4 cont'd's SELinux policy (selinux/classes.conf +
# selinux/policy.conf, loaded at boot by rootfs/init/main.go's
# loadSELinuxPolicy) is genuinely complete, not just quietly permissive -
# the exact allow-rule set was only ever arrived at empirically, boot by
# boot, reading real AVC denials out of dmesg and fixing exactly what
# each one named (see selinux/policy.conf's own header and
# docs/architecture.md for the class-of-bug list that turned up this
# way: missing self:fd use, missing filesystem:associate on every single
# fs_use-labeled type, missing process:{noatsecure,rlimitinh,siginh} on
# every domain transition, missing peer/packet class rules for
# unlabeled/default-context network traffic under policycap
# always_check_network - none of which is guessable from reading the
# rule set alone, all of which showed up as a real "avc: denied" line on
# a real boot).
#
# Runs two boots against the same squashfs+dm-verity image (see
# hack/qemu-verity-boot-test.sh, which this shares its boot pattern
# with):
#   1. the kernel's own default (kernel/configs/haproxyos_defconfig's
#      SECURITY_SELINUX_DEVELOP=y keeps this permissive unless told
#      otherwise) - must show the policy actually loading
#      ("init: selinux: loaded policy"), must show real HTTP 200 (proves
#      loading the policy didn't break anything), and must show zero
#      "avc: denied" lines - a permissive-mode denial doesn't block
#      anything, so a clean boot with real traffic passing is NOT enough
#      on its own to prove the rule set is complete; only an empty grep
#      for denials is.
#   2. the identical image, `enforcing=1` added to the cmdline - the
#      real test of whether the rule set is actually sufficient, not
#      just quiet: SELinux now genuinely blocks anything not allowed.
#      Must show the same real HTTP 200 and the same zero denials - a
#      single missing allow rule here would make something actually
#      fail (a mount, an exec, a socket operation), not just log.
#
# SELinux stays permissive by *default* in the shipped kernel config
# even though this test proves enforcing already works cleanly -
# flipping the shipped default is a separate, deliberate decision this
# slice doesn't make on its own (see docs/architecture.md).
#
# Usage: hack/qemu-selinux-test.sh <bzImage> <rootfs-dir>
# <rootfs-dir> must contain rootfs.squashfs, rootfs.verity,
# rootfs.roothash and rootfs.verity.info (see rootfs/assemble.sh), built
# with the SELinux policy bundled in (see the Makefile's rootfs-build
# target - it always includes it, there's no separate opt-out build).
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <rootfs-dir>}"
ROOTFS_DIR="${2:?usage: $0 <bzImage> <rootfs-dir>}"
HTTP_TIMEOUT_SECS="${QEMU_SELINUX_HTTP_TIMEOUT:-30}"

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"

dm_table() {
  "$(dirname "$0")/dm-verity-cmdline.sh" "$ROOTFS_DIR" /dev/vda /dev/vdb
}

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# boot_and_check <label> <extra-cmdline> <host-port>
boot_and_check() {
  local label="$1" extra_cmdline="$2" host_port="$3"
  local log="$WORKDIR/$label.log"

  qemu-system-x86_64 \
    -kernel "$KERNEL" \
    -append "console=ttyS0 panic=-1 dm-mod.create=\"$(dm_table)\" root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp $extra_cmdline" \
    -nographic -no-reboot -display none -m 256M \
    -drive file="$SQUASHFS",format=raw,if=virtio,readonly=on \
    -drive file="$VERITY",format=raw,if=virtio,readonly=on \
    -netdev "user,id=net0,hostfwd=tcp::${host_port}-:8080" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$log" \
    &
  QEMU_PID=$!

  local deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
  local code=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${host_port}/" || true)"
    [ "$code" = "200" ] && break
    sleep 1
  done

  kill "$QEMU_PID" 2>/dev/null || true
  wait "$QEMU_PID" 2>/dev/null || true
  QEMU_PID=""

  if [ "$code" != "200" ]; then
    echo "SELinux test FAILED ($label): no HTTP 200 within ${HTTP_TIMEOUT_SECS}s (last code: '${code}')" >&2
    echo "--- console output ---" >&2; cat "$log" >&2
    exit 1
  fi
  if ! grep -qF "init: selinux: loaded policy" "$log"; then
    echo "SELinux test FAILED ($label): got HTTP 200, but the policy never loaded - this boot proved nothing about SELinux at all" >&2
    echo "--- console output ---" >&2; cat "$log" >&2
    exit 1
  fi
  if grep -q "avc:.*denied" "$log"; then
    echo "SELinux test FAILED ($label): got HTTP 200, but at least one AVC denial occurred:" >&2
    grep "avc:.*denied" "$log" >&2
    echo "--- console output ---" >&2; cat "$log" >&2
    exit 1
  fi
  echo "SELinux test OK ($label): policy loaded, HTTP 200, zero AVC denials"
}

boot_and_check permissive "" 18091
boot_and_check enforcing "enforcing=1" 18092

echo "SELinux policy test OK: the real, hand-written policy loads and mediates a full boot to a working HTTP 200, with zero denials, both permissively (the shipped default) and with enforcing=1 (proving the rule set is actually complete, not just quiet)"
