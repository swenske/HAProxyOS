#!/usr/bin/env bash
# Proves LifecycleService.Rollback (internal/api/lifecycle.go) actually
# works, end to end, over a real gRPC call against a running node - not
# just that the underlying ESP-swap mechanism works when driven
# directly by image/disk/activate-slot.sh (see
# hack/qemu-uefi-ab-boot-test.sh, which stays as the narrower,
# build-tool-level proof of that).
#
#   1. boot disk.img (image/disk/assemble.sh, slot A active) under real
#      OVMF firmware - HTTP 200 confirms it's up.
#   2. extract ca.crt/admin.crt/admin.key straight from disk.img's
#      STATE partition (partition 6) via `debugfs dump`, entirely
#      offline (no mount, no loop device, same reasoning as
#      hack/qemu-state-persist-test.sh's own debugfs use) - haproxyosd
#      only ever prints the admin cert/key to the *console* once, and
#      there's no CA cert there at all to trust a fresh connection with
#      (by design - see cmd/haproxyosd/main.go), so this reads them the
#      way a real out-of-band provisioning step would: from the STATE
#      partition directly, before or without ever touching the console.
#   3. call `haproxyosctl lifecycle rollback` over real mTLS, against
#      the running node's gRPC port - must return "active slot: B".
#   4. this QEMU instance is run WITHOUT -no-reboot, unlike every other
#      boot test here: the whole point is to watch the guest actually
#      reboot itself (Rollback triggers a real syscall.Reboot) and come
#      back up through real OVMF firmware discovery a second time,
#      inside the same process - proving this is a genuine, complete
#      reboot cycle, not just that the RPC returned successfully.
#   5. after the second boot, HTTP must answer again, the console's own
#      dm-mod.create= line must now reference /dev/vda4 (slot B, not
#      slot A), and haproxyosd's "first boot" line must NOT appear
#      again - STATE (the CA the first boot generated) survived the
#      reboot, same invariant hack/qemu-uefi-ab-boot-test.sh already
#      proves for the build-tool-driven switch, now proved for the
#      real API-driven one too.
#
# Usage: hack/qemu-lifecycle-rollback-test.sh <disk.img> <haproxyosctl-bin>
set -euo pipefail

# debugfs (e2fsprogs) installs to /usr/sbin, same PATH gap already hit
# for veritysetup/mkfs.ext4/mkfs.vfat/sgdisk on haproxyos-runner01.
export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <haproxyosctl-bin>}"
CTL="${2:?usage: $0 <disk.img> <haproxyosctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_ROLLBACK_HTTP_TIMEOUT:-40}"
REBOOT_TIMEOUT_SECS="${QEMU_ROLLBACK_REBOOT_TIMEOUT:-60}"
HOST_HTTP_PORT="${QEMU_ROLLBACK_HTTP_PORT:-18090}"
HOST_GRPC_PORT="${QEMU_ROLLBACK_GRPC_PORT:-18091}"
MARKER="HAPROXYOS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# --- boot slot A for real (no -no-reboot: this VM must be allowed to
# actually reboot itself later) - PKI doesn't exist on a freshly
# assembled disk's STATE partition at all until a real boot bootstraps
# it, so this has to happen before any extraction below.
OVMF_VARS="$WORKDIR/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
LOG="$WORKDIR/console.log"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_HTTP_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" != "200" ]; then
  echo "Rollback test FAILED: slot A never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if ! grep -q "$FIRST_BOOT_MSG" "$LOG"; then
  echo "Rollback test FAILED: slot A didn't log '$FIRST_BOOT_MSG' - expected a fresh bootstrap" >&2
  exit 1
fi
echo "Slot A OK: real UEFI boot, HTTP 200, PKI bootstrapped"

# --- extract PKI material straight from disk.img's STATE partition -
# safe to read concurrently with the still-running guest: haproxyosd's
# PKI bootstrap calls syscall.Sync() before it even starts listening
# (see cmd/haproxyosd/main.go), and HTTP 200 above already proves the
# listener is up, so those writes are already visible through the host
# page cache backing this same disk.img file. ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none

for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Rollback test FAILED: couldn't extract pki/$f from disk.img's STATE partition after slot A booted" >&2; exit 1; }
done

# --- call Rollback for real, over mTLS ---
ROLLBACK_OUT="$("$CTL" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" lifecycle rollback)"
echo "$ROLLBACK_OUT"
if ! echo "$ROLLBACK_OUT" | grep -q "slot B"; then
  echo "Rollback test FAILED: haproxyosctl lifecycle rollback didn't report switching to slot B" >&2
  exit 1
fi

# --- wait for the guest to actually reboot and come back up ---
deadline=$((SECONDS + REBOOT_TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)"
  [ "$code" = "200" ] && [ "$(grep -c "$MARKER" "$LOG" 2>/dev/null || true)" -ge 2 ] && break
  sleep 1
done

kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

if [ "$code" != "200" ]; then
  echo "Rollback test FAILED: no HTTP 200 after the reboot within ${REBOOT_TIMEOUT_SECS}s - did the guest actually reboot?" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if [ "$(grep -c "$MARKER" "$LOG")" -lt 2 ]; then
  echo "Rollback test FAILED: $MARKER appeared fewer than twice - this wasn't a genuine second boot" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if ! grep -q "dm-mod.create=.*verity 1 /dev/vda4 /dev/vda5" "$LOG"; then
  echo "Rollback test FAILED: console cmdline never referenced BOOT-B-DATA/HASH (/dev/vda4+5) - the ESP swap Rollback performed didn't actually take effect on reboot" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if [ "$(grep -c "$FIRST_BOOT_MSG" "$LOG")" -ne 1 ]; then
  echo "Rollback test FAILED: '$FIRST_BOOT_MSG' appeared $(grep -c "$FIRST_BOOT_MSG" "$LOG" 2>/dev/null || echo 0) times, want exactly 1 (only the first boot) - STATE didn't survive the Rollback-triggered reboot" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi

echo "Rollback test OK: a real gRPC LifecycleService.Rollback call made the node reboot itself into slot B, with STATE (PKI) surviving intact"
