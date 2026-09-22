#!/usr/bin/env bash
# Boots a built HAProxyOS kernel+initramfs under QEMU and checks for the
# init's success marker on the (serial) console - the Phase 1 boot-proof
# test. Exits 0 iff HAPROXYOS_INIT_BOOT_OK appears before the timeout.
#
# Usage: hack/qemu-run.sh <bzImage> <initramfs.cpio.gz>
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <initramfs.cpio.gz>}"
INITRD="${2:?usage: $0 <bzImage> <initramfs.cpio.gz>}"
TIMEOUT_SECS="${QEMU_BOOT_TIMEOUT:-30}"
MARKER="HAPROXYOS_INIT_BOOT_OK"

LOG="$(mktemp)"
trap 'rm -f "$LOG"' EXIT

timeout "${TIMEOUT_SECS}" qemu-system-x86_64 \
  -kernel "$KERNEL" \
  -initrd "$INITRD" \
  -append "console=ttyS0 panic=-1" \
  -nographic -no-reboot -m 256M \
  -serial mon:stdio \
  >"$LOG" 2>&1 || true

if grep -q "$MARKER" "$LOG"; then
  echo "Boot OK: found $MARKER"
  exit 0
fi

echo "Boot FAILED: $MARKER not found in console output within ${TIMEOUT_SECS}s" >&2
echo "--- console output ---" >&2
cat "$LOG" >&2
exit 1
