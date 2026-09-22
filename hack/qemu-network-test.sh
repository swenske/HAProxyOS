#!/usr/bin/env bash
# Boots a kernel+initramfs under QEMU with virtio-net + DHCP (kernel
# ip=dhcp, no userspace network tooling) and a host port forwarded to the
# guest's HAProxy health frontend (see rootfs/base/etc/haproxy/
# haproxy.cfg), then polls it over real TCP/IP until it answers or a
# timeout is hit. This is the Phase 2 network-integration boot test: it
# proves haproxyosd -> haproxy actually run and serve traffic *inside*
# the QEMU-booted kernel, not just on the build host (see hack/
# qemu-run.sh and the "HAProxy integration test" step in image-build.yml
# for that host-level check).
#
# Usage: hack/qemu-network-test.sh <bzImage> <initramfs.cpio.gz>
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <initramfs.cpio.gz>}"
INITRD="${2:?usage: $0 <bzImage> <initramfs.cpio.gz>}"
HOST_PORT="${QEMU_NET_TEST_PORT:-18080}"
TIMEOUT_SECS="${QEMU_NET_TEST_TIMEOUT:-30}"

LOG="$(mktemp)"
trap 'rm -f "$LOG"; [ -n "${QEMU_PID:-}" ] && kill "$QEMU_PID" 2>/dev/null || true' EXIT

qemu-system-x86_64 \
  -kernel "$KERNEL" \
  -initrd "$INITRD" \
  -append "console=ttyS0 panic=-1 ip=dhcp" \
  -nographic -no-reboot -display none -m 256M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT}/" || true)"
  if [ "$code" = "200" ]; then
    echo "Network boot test OK: HTTP 200 from HAProxy inside the VM"
    exit 0
  fi
  sleep 1
done

echo "Network boot test FAILED: no HTTP 200 within ${TIMEOUT_SECS}s (last code: '${code}')" >&2
echo "--- console output ---" >&2
cat "$LOG" >&2
exit 1
