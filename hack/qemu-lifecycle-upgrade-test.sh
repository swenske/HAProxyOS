#!/usr/bin/env bash
# Proves LifecycleService.Upgrade actually installs a *genuinely new*
# rootfs onto the inactive slot over a real gRPC call, not just that it
# can re-stage the same content Rollback already covers:
#
#   1. boot the existing disk.img (image/disk/assemble.sh, slot A
#      active, bootstrap config on :8080) under real OVMF - HTTP 200
#      confirms it's up, PKI bootstraps onto STATE.
#   2. build a second, genuinely different rootfs (same init/
#      haproxyosd/haproxy binaries, a haproxy.cfg bound to :8081
#      instead of :8080 - different content, different squashfs, a
#      different root hash) and a release bundle for it (image/
#      release/assemble.sh - rootfs.squashfs/rootfs.verity/uki-a.efi/
#      uki-b.efi).
#   3. inject the v2 bundle's 4 files into disk.img's STATE partition
#      *before ever booting it* (under a new "upgrade/" directory,
#      via debugfs -w), then extract ca.crt/admin.crt/admin.key back
#      out the same way, after boot A (haproxyosd never prints a CA
#      cert to the console at all, by design). Both have to happen
#      through debugfs directly on the image file, never through a
#      live mount from the host: haproxyosctl (host) and haproxyosd
#      (guest) don't share a filesystem across the QEMU host/guest
#      boundary, unlike CLAUDE.md's normal "share a filesystem" case -
#      and writing to STATE from the host while the guest *also* has
#      it mounted read-write would just corrupt it (this is exactly why
#      the injection happens before the first boot, not concurrently
#      with a running one).
#   4. call `haproxyosctl lifecycle upgrade` over real mTLS, pointing
#      at the bundle's path *as the guest itself sees it*
#      (/etc/.state/upgrade, since rootfs/init/main.go's mountState
#      mounts all of STATE there) - Upgrade must write the v2
#      squashfs/verity to slot B's partitions (slot A is active, so B
#      is the target), switch the ESP to slot B's (v2) UKI, and reboot
#      - all without touching STATE or slot A's own content at all.
#   5. after the guest reboots itself (no -no-reboot, same as the
#      rollback test), it must come back up healthy (HTTP 200 on
#      :8080 again - *not* :8081, see the note below) with PKI's
#      "first boot" line NOT reappearing (STATE survived), and the
#      kernel's own "Kernel command line:" log line must show it
#      booted from slot B's partitions (/dev/vda4 /dev/vda5) with v2's
#      root hash, not slot A's.
#   6. as a bonus check that Upgrade didn't corrupt the *other* slot's
#      own staging: call Rollback afterward and confirm the guest comes
#      back up healthy again, with the kernel cmdline now showing slot
#      A's partitions (/dev/vda2 /dev/vda3) and v1's root hash again -
#      proving slot A's pre-existing \HAPROXYOS\UKI-A.EFI is still
#      intact and correct.
#
# Real bug found empirically while writing this test (not a production
# bug - a wrong assumption in the test's *own* first draft, which
# expected v2's bootstrap config on :8081 to actually get served after
# the upgrade): rootfs/init/main.go's seedPersistentHaproxyCfg only
# seeds STATE's haproxy/haproxy.cfg from the booting rootfs's own
# bootstrap default *if STATE doesn't already have one* - and STATE is
# one partition shared by both A/B slots, not duplicated per slot. By
# the time slot B ever boots, slot A's first boot has already persisted
# its own (:8080) config onto that shared STATE, so slot B's bind-
# mounted /etc/haproxy still serves *that*, never its own squashfs's
# bootstrap default - confirmed by booting the post-upgrade disk.img
# directly and finding :8080 (not :8081) answering. This matches the
# system's actual, intended design (an upgrade must never reset a
# node's live-applied HAProxy config back to some bootstrap default),
# so what changed here is the test's verification method, not
# lifecycle.go or main.go: proof that *genuinely new content* is
# running has to come from the kernel cmdline's root hash and slot
# device paths (both verified below), not from which HTTP port
# answers, since the served config is deliberately state-persisted and
# identical across both slots once slot A has booted once.
#
# Usage: hack/qemu-lifecycle-upgrade-test.sh <disk.img> <bzImage> <build-dir> <haproxyosctl-bin>
# <disk.img> is image/disk/assemble.sh's output for the *original* (v1,
# :8080) image, mutated in place by this test. <build-dir> is the
# Makefile's own $(BUILD_DIR) - must contain init/haproxyosd/haproxy
# (rootfs/assemble.sh's own inputs, reused here to build a v2 rootfs)
# and rootfs/{rootfs.squashfs,rootfs.verity,rootfs.roothash,
# rootfs.verity.info} (rootfs/assemble.sh's output for v1).
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <bzImage> <build-dir> <haproxyosctl-bin>}"
KERNEL="${2:?usage: $0 <disk.img> <bzImage> <build-dir> <haproxyosctl-bin>}"
BUILD_DIR="${3:?usage: $0 <disk.img> <bzImage> <build-dir> <haproxyosctl-bin>}"
CTL="${4:?usage: $0 <disk.img> <bzImage> <build-dir> <haproxyosctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_UPGRADE_HTTP_TIMEOUT:-40}"
REBOOT_TIMEOUT_SECS="${QEMU_UPGRADE_REBOOT_TIMEOUT:-60}"
HOST_PORT_8080="${QEMU_UPGRADE_TEST_PORT:-18092}"
HOST_PORT_8081="${QEMU_UPGRADE_TEST_PORT2:-18093}"
HOST_GRPC_PORT="${QEMU_UPGRADE_GRPC_PORT:-18094}"
MARKER="HAPROXYOS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# --- build a genuinely different v2 rootfs + release bundle ---
cat > "$WORKDIR/haproxy-v2.cfg" <<'EOF'
global
    stats socket /run/haproxyos/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000

defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend haproxyos-health
    bind *:8081
    http-request return status 200 content-type text/plain string "HAProxyOS: v2 is up\n"
EOF

V2_ROOTFS="$WORKDIR/rootfs-v2"
mkdir -p "$V2_ROOTFS"
"$SELF_DIR/../rootfs/assemble.sh" "$V2_ROOTFS" "$BUILD_DIR/init" "$BUILD_DIR/haproxyosd" \
  "$BUILD_DIR/haproxy" "$WORKDIR/haproxy-v2.cfg"

V2_BUNDLE="$WORKDIR/bundle-v2"
"$SELF_DIR/../image/release/assemble.sh" "$V2_BUNDLE" "$KERNEL" "$V2_ROOTFS"

# --- inject the v2 bundle into disk.img's STATE partition, before the
# first boot (see this script's own header comment for why it has to
# be before, not while the guest has it mounted) ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
debugfs -w -R "mkdir upgrade" "$STATE_IMG" >/dev/null 2>&1
for f in rootfs.squashfs rootfs.verity "uki-b.efi"; do
  debugfs -w -R "write $V2_BUNDLE/$f upgrade/$f" "$STATE_IMG" >/dev/null 2>&1
done
dd if="$STATE_IMG" of="$DISK" bs=512 seek="$STATE_START_SECTOR" conv=notrunc status=none
GUEST_BUNDLE_PATH="/etc/.state/upgrade"
echo "Injected v2 release bundle into disk.img's STATE partition (guest path: $GUEST_BUNDLE_PATH)"

# --- boot slot A (v1, :8080) for real ---
boot_disk() {
  local log="$1"
  local ovmf_vars="$WORKDIR/OVMF_VARS-$(basename "$log").fd"
  cp "$OVMF_VARS_TEMPLATE" "$ovmf_vars"

  qemu-system-x86_64 \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$ovmf_vars" \
    -drive file="$DISK",format=raw,if=virtio \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_PORT_8081}-:8081,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$log" \
    &
  QEMU_PID=$!
}

wait_http() {
  local port="$1" timeout_secs="$2" code=""
  local deadline=$((SECONDS + timeout_secs))
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/" || true)"
    [ "$code" = "200" ] && return 0
    sleep 1
  done
  return 1
}

# wait_http_and_marker is wait_http plus a minimum boot-marker count,
# both required together in the *same* poll - same pattern hack/
# qemu-lifecycle-rollback-test.sh already uses, and for the same
# reason: :8080 keeps answering right up until the 2-second-delayed
# reboot actually happens (scheduleReboot in internal/api/lifecycle.go
# sleeps before rebooting so the RPC's own response can still reach the
# caller), so a bare "HTTP 200" check can pass on the *old* boot's
# still-live process, before the genuine second boot ever occurred -
# requiring the marker count too closes that race.
wait_http_and_marker() {
  local port="$1" log="$2" want_markers="$3" timeout_secs="$4" code=""
  local deadline=$((SECONDS + timeout_secs))
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/" || true)"
    if [ "$code" = "200" ] && [ "$(grep -c "$MARKER" "$log" 2>/dev/null || true)" -ge "$want_markers" ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# assert_last_boot checks that the *most recent* boot recorded in a
# console log genuinely ran from the expected slot's own partitions
# with the expected rootfs's root hash - the only reliable way to tell
# slot A and slot B's content apart once the guest has booted (see this
# script's header comment: the served HTTP config is deliberately
# state-persisted and identical across both slots after slot A's first
# boot, so it can't be used as a signal here). "Kernel command line:"
# is the kernel's own canonical, single-per-boot restatement of
# /proc/cmdline (as opposed to the earlier EFI-stub "Command line:"
# line, which duplicates it) - tail -1 picks the latest boot's, not an
# earlier one still sitting in the same accumulated log file.
V1_HASH="$(cat "$BUILD_DIR/rootfs/rootfs.roothash")"
V2_HASH="$(cat "$V2_BUNDLE/rootfs.roothash")"
assert_last_boot() {
  local log="$1" data_dev="$2" hash_dev="$3" want_hash="$4" label="$5"
  local line
  line="$(grep "^Kernel command line:" "$log" | tail -1)"
  if [ -z "$line" ]; then
    echo "Upgrade test FAILED: $label - no 'Kernel command line:' line found in the console log at all" >&2
    echo "--- console output ---" >&2; cat "$log" >&2
    exit 1
  fi
  case "$line" in
    *"verity 1 $data_dev $hash_dev "*) : ;;
    *) echo "Upgrade test FAILED: $label - last boot's cmdline doesn't reference $data_dev/$hash_dev: $line" >&2; exit 1 ;;
  esac
  case "$line" in
    *"$want_hash"*) : ;;
    *) echo "Upgrade test FAILED: $label - last boot's cmdline doesn't carry root hash $want_hash: $line" >&2; exit 1 ;;
  esac
}

A_LOG="$WORKDIR/boot-a.log"
boot_disk "$A_LOG"
if ! wait_http "$HOST_PORT_8080" "$HTTP_TIMEOUT_SECS"; then
  echo "Upgrade test FAILED: slot A (v1, :8080) never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
if ! grep -q "$FIRST_BOOT_MSG" "$A_LOG"; then
  echo "Upgrade test FAILED: slot A didn't log '$FIRST_BOOT_MSG' - expected a fresh bootstrap" >&2
  exit 1
fi
assert_last_boot "$A_LOG" /dev/vda2 /dev/vda3 "$V1_HASH" "initial boot"
echo "Slot A (v1) OK: real UEFI boot, HTTP 200 on :8080, PKI bootstrapped, root hash $V1_HASH confirmed"

# --- extract PKI material straight from disk.img's STATE partition
# (same sector range computed above - partition geometry doesn't
# change between boots) ---
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Upgrade test FAILED: couldn't extract pki/$f from disk.img's STATE partition after slot A booted" >&2; exit 1; }
done

# --- call Upgrade for real, over mTLS, referencing the bundle's
# *guest-side* path - haproxyosctl's own -sha256 auto-detection reads
# from the argument it's given, which would be the guest path here (not
# readable from the host running haproxyosctl), so the sha256 is passed
# explicitly instead, computed from the real, host-local bundle. ---
V2_SHA256="$(cat "$V2_BUNDLE/rootfs.squashfs.sha256")"
UPGRADE_OUT="$("$CTL" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" lifecycle upgrade -sha256 "$V2_SHA256" "$GUEST_BUNDLE_PATH")"
echo "$UPGRADE_OUT"
if ! echo "$UPGRADE_OUT" | grep -qi "rebooting"; then
  echo "Upgrade test FAILED: haproxyosctl lifecycle upgrade never reached the 'rebooting' stage" >&2
  exit 1
fi

# --- wait for the guest to reboot and come back up healthy - :8080,
# not :8081: the served config comes from STATE, shared across both
# slots and already persisted by slot A's first boot, so it stays
# :8080 after the upgrade too (see this script's header comment).
# Requires the marker count in the *same* poll (wait_http_and_marker),
# not a bare HTTP check, or a still-live pre-reboot response can race
# past a check that only looks at HTTP 200. ---
if ! wait_http_and_marker "$HOST_PORT_8080" "$A_LOG" 2 "$REBOOT_TIMEOUT_SECS"; then
  echo "Upgrade test FAILED: no healthy, genuinely-rebooted HTTP 200 on :8080 within ${REBOOT_TIMEOUT_SECS}s - did the guest actually reboot into the upgraded slot?" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
if [ "$(grep -c "$FIRST_BOOT_MSG" "$A_LOG")" -ne 1 ]; then
  echo "Upgrade test FAILED: '$FIRST_BOOT_MSG' appeared $(grep -c "$FIRST_BOOT_MSG" "$A_LOG" 2>/dev/null || echo 0) times, want exactly 1 - STATE didn't survive the upgrade-triggered reboot" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
assert_last_boot "$A_LOG" /dev/vda4 /dev/vda5 "$V2_HASH" "post-upgrade boot"
echo "Slot B (v2) OK: real gRPC Upgrade wrote the new rootfs (root hash $V2_HASH, /dev/vda4+/dev/vda5), switched, and rebooted into it - HTTP healthy, STATE intact"

kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

# --- bonus: Rollback afterward must still correctly switch back to
# the untouched, original slot A (v1) - HTTP still answers on :8080
# either way (same shared, state-persisted config), so the real proof
# is the kernel cmdline reverting to slot A's own partitions and root
# hash again ---
B_LOG="$WORKDIR/boot-b.log"
boot_disk "$B_LOG"
if ! wait_http "$HOST_PORT_8080" "$HTTP_TIMEOUT_SECS"; then
  echo "Upgrade test FAILED: slot B (v2) didn't come back up for the Rollback bonus check" >&2
  exit 1
fi
for f in ca.crt admin.crt admin.key; do rm -f "$WORKDIR/$f"; done
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
done
"$CTL" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" lifecycle rollback

if ! wait_http_and_marker "$HOST_PORT_8080" "$B_LOG" 2 "$REBOOT_TIMEOUT_SECS"; then
  echo "Upgrade test FAILED: Rollback after the upgrade never brought a genuinely rebooted slot A back up - slot A's own staged UKI may have been disturbed by Upgrade" >&2
  echo "--- console output ---" >&2; cat "$B_LOG" >&2
  exit 1
fi
assert_last_boot "$B_LOG" /dev/vda2 /dev/vda3 "$V1_HASH" "post-rollback boot"
echo "Rollback-after-Upgrade OK: slot A's original content (root hash $V1_HASH, /dev/vda2+/dev/vda3) is still intact and reachable"
echo "Upgrade test OK: a real gRPC LifecycleService.Upgrade call installed a genuinely new rootfs, and the untouched slot remained rollback-able afterward"
