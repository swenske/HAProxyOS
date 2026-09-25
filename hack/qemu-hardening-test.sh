#!/usr/bin/env bash
# Proves Phase 4's runtime kernel hardening (rootfs/init/main.go's
# hardenSysctls) actually applies every sysctl it claims to, on a real
# boot - not just that the Go code doesn't panic, and not just that the
# matching kernel/configs/janus_defconfig options compile in (a
# real gap this test's own first draft caught: CONFIG_SYN_COOKIES
# wasn't set, so /proc/sys/net/ipv4/tcp_syncookies didn't exist at all
# and that one write silently logged "no such file or directory" while
# every other sysctl succeeded - the boot itself, and even the network
# test, looked completely fine either way, since a non-fatal write
# failure doesn't block anything). Same boot pattern hack/
# qemu-network-test.sh uses (janusd -> haproxy inside the VM,
# real HTTP over virtio-net), plus a check of every "init: sysctl ..."
# console line hardenSysctls prints - each one is logged as it's
# written, success or failure, specifically so an external test like
# this one can verify the actual outcome without needing a shell inside
# the guest to read /proc/sys back.
#
# Usage: hack/qemu-hardening-test.sh <bzImage> <initramfs.cpio.gz>
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <initramfs.cpio.gz>}"
INITRD="${2:?usage: $0 <bzImage> <initramfs.cpio.gz>}"
HOST_PORT="${QEMU_HARDENING_TEST_PORT:-18099}"
TIMEOUT_SECS="${QEMU_HARDENING_TEST_TIMEOUT:-30}"

# Every sysctl rootfs/init/main.go's hardenSysctls sets, and the value
# it's expected to end up with - kept in sync with that function by
# hand (no way to import Go constants into a shell script), so a real
# boot's console output is checked against this list line for line.
declare -A EXPECTED=(
  ["/proc/sys/fs/protected_hardlinks"]="1"
  ["/proc/sys/fs/protected_symlinks"]="1"
  ["/proc/sys/kernel/dmesg_restrict"]="1"
  ["/proc/sys/kernel/kptr_restrict"]="2"
  ["/proc/sys/kernel/yama/ptrace_scope"]="2"
  ["/proc/sys/net/ipv4/conf/all/accept_redirects"]="0"
  ["/proc/sys/net/ipv4/conf/all/accept_source_route"]="0"
  ["/proc/sys/net/ipv4/conf/all/rp_filter"]="1"
  ["/proc/sys/net/ipv4/conf/all/send_redirects"]="0"
  ["/proc/sys/net/ipv4/conf/default/accept_redirects"]="0"
  ["/proc/sys/net/ipv4/conf/default/accept_source_route"]="0"
  ["/proc/sys/net/ipv4/conf/default/rp_filter"]="1"
  ["/proc/sys/net/ipv4/conf/default/send_redirects"]="0"
  ["/proc/sys/net/ipv4/icmp_echo_ignore_broadcasts"]="1"
  ["/proc/sys/net/ipv4/icmp_ignore_bogus_error_responses"]="1"
  ["/proc/sys/net/ipv4/tcp_syncookies"]="1"
)

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
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" != "200" ]; then
  echo "Hardening test FAILED: no HTTP 200 within ${TIMEOUT_SECS}s (last code: '${code}') - hardening shouldn't have broken this" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "HAProxy OK: HTTP 200 - hardening didn't break normal boot/serving"

# A moment for the last sysctl writes (which happen before HAProxy even
# starts) to have long since flushed to the log file.
sleep 1

fail=0
for path in "${!EXPECTED[@]}"; do
  want="${EXPECTED[$path]}"
  if grep -qF "init: sysctl ${path}=${want}" "$LOG"; then
    continue
  fi
  if grep -qF "init: sysctl ${path}=" "$LOG"; then
    echo "Hardening test FAILED: $path was set, but not to the expected value $want:" >&2
    grep -F "init: sysctl ${path}=" "$LOG" >&2
  else
    echo "Hardening test FAILED: $path was never even attempted (no console line for it at all)" >&2
  fi
  fail=1
done
if [ "$fail" -ne 0 ]; then
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "All ${#EXPECTED[@]} hardening sysctls confirmed set to their expected values on a real boot"

# The one failure mode this test exists specifically to catch: a
# sysctl write that *failed* (a missing /proc/sys node - e.g. the real
# CONFIG_SYN_COOKIES gap this test's own first draft found) would still
# leave every check above passing for every *other* sysctl, so also
# fail loudly on any "init: sysctl ...: <error>" line at all, expected
# or not.
if grep -qE "^init: sysctl .*: [a-zA-Z]" "$LOG"; then
  echo "Hardening test FAILED: at least one sysctl write logged an error:" >&2
  grep -E "^init: sysctl .*: [a-zA-Z]" "$LOG" >&2
  exit 1
fi
echo "Hardening test OK: every kernel hardening sysctl this project sets took effect on a real boot, and HAProxy still serves traffic normally"
