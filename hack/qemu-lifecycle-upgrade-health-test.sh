#!/usr/bin/env bash
# Proves LifecycleService.Upgrade's wait_for_health actually confirms a
# healthy new slot and leaves it running - and, symmetrically, actually
# reverts and reboots back to the previous slot automatically when the
# new one never becomes healthy - both without any RPC call driving the
# revert itself: nobody is watching by then, the original Upgrade call's
# connection died with the first reboot. See internal/bootcommit's
# package doc and internal/api/lifecycle.go's Upgrade doc comment for
# the full mechanism this exercises end to end.
#
#   1. boot the existing disk.img (slot A active) for real, confirm
#      HTTP 200, extract PKI creds from STATE via debugfs (same
#      reasoning as hack/qemu-lifecycle-upgrade-test.sh: a script can't
#      watch a live console the way a human doing this for real would).
#   2. build two release bundles and inject *both* into disk.img's
#      STATE partition before ever booting it (same debugfs-before-
#      first-boot reasoning as hack/qemu-lifecycle-upgrade-test.sh -
#      janusctl and janusd don't share a filesystem across this
#      QEMU host/guest boundary):
#        - "good": just image/release/assemble.sh run against the
#          *existing* v1 rootfs build output - genuinely new content
#          isn't the point of this test (hack/qemu-lifecycle-
#          upgrade-test.sh already proves Upgrade installs genuinely
#          new content; this one is about the health-check/revert
#          mechanics), so there's no need to build a second rootfs for
#          this half.
#        - "broken": a *fresh* rootfs built with the host's own
#          dynamically-linked /bin/false standing in for janusd
#          (rootfs/assemble.sh's own <janusd-bin> argument takes
#          anything executable). This rootfs ships no dynamic linker or
#          libc at all (every real binary in it - init, janusd,
#          haproxy - is statically linked, by design), so Supervisor
#          doesn't even get as far as exec succeeding: every restart
#          attempt fails at spawn() itself ("no such file or directory"
#          for the missing ELF interpreter) - a realistic simulation of
#          a badly-built or wrong-architecture control-plane binary
#          landing in a release bundle, and it exercises exactly the
#          spawn-failure path of Supervisor.runOnce, not just the
#          child-exits-immediately one.
#        - "haproxy-broken": a *third* fresh rootfs with the real,
#          working init and janusd, but /bin/false standing in for
#          *haproxy* itself this time (rootfs/assemble.sh's
#          <haproxy-bin> argument, not <janusd-bin>) - janusd
#          starts up perfectly fine (it's the real binary), calls
#          internal/haproxy.Manager.Reload(), which fails and is logged
#          but not fatal (matching cmd/janusd/main.go's existing
#          "continue without it" behavior for a missing/broken haproxy),
#          and HAProxy's stats socket simply never comes into existence
#          - so every ShowInfo() call inside
#          cmd/janusd's own confirmBootHealth goroutine fails
#          forever. This is what actually exercises the real,
#          HAProxy-level health check this test is named for - the
#          *other* "broken" bundle above only ever exercises rootfs/
#          init's own Supervisor-level backstop (janusd itself never
#          running at all), a different, complementary failure mode.
#   3. calls `janusctl lifecycle upgrade -wait-for-health
#      -health-timeout 5` with the "good" bundle - the guest reboots
#      into slot B for real, and after health-timeout-plus-a-margin, the
#      console must show "bootcommit: confirmed healthy" and the kernel
#      cmdline must still show slot B's partitions/root hash - no revert.
#   4. calls the same RPC again with the "broken" (daemon-broken)
#      bundle, targeting whichever slot is inactive at that point (A,
#      since step 3 left B active) - the guest reboots into slot A,
#      janusd (really /bin/false) crash-loops immediately, and -
#      entirely on its own, no RPC involved - Supervisor's GiveUpAfter
#      elapses and the node reverts and reboots a *second* time, this
#      time back to slot B (RevertTo was recorded as whatever was active
#      *when the broken Upgrade was called*, i.e. B - not hardcoded back
#      to the original slot A) - proving the revert target is dynamic,
#      not a fixed fallback. HTTP must come back up on its own
#      afterward.
#   5. calls the same RPC a third time with the "haproxy-broken" bundle,
#      targeting whichever slot is inactive at that point (A again,
#      since step 4 left B active) - the guest reboots into slot A for
#      real (proving *this* rootfs, unlike the daemon-broken one, boots
#      and runs janusd just fine), and - again with no RPC involved,
#      this time via cmd/janusd's own confirmBootHealth rather than
#      rootfs/init's GiveUpAfter - reverts and reboots a second time,
#      back to slot B once more.
#
# Usage: hack/qemu-lifecycle-upgrade-health-test.sh <disk.img> <bzImage> <build-dir> <janusctl-bin>
# Same inputs as hack/qemu-lifecycle-upgrade-test.sh.
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
KERNEL="${2:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
BUILD_DIR="${3:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
CTL="${4:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_UPGRADE_HEALTH_HTTP_TIMEOUT:-40}"
REBOOT_TIMEOUT_SECS="${QEMU_UPGRADE_HEALTH_REBOOT_TIMEOUT:-60}"
HEALTH_TIMEOUT_SECS="${QEMU_UPGRADE_HEALTH_TIMEOUT:-5}"
HOST_PORT_8080="${QEMU_UPGRADE_HEALTH_TEST_PORT:-18095}"
HOST_GRPC_PORT="${QEMU_UPGRADE_HEALTH_GRPC_PORT:-18096}"
MARKER="JANUS_INIT_BOOT_OK"

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

# --- build the two release bundles ---
GOOD_BUNDLE="$WORKDIR/bundle-good"
"$SELF_DIR/../image/release/assemble.sh" "$GOOD_BUNDLE" "$KERNEL" "$BUILD_DIR/rootfs"

BROKEN_ROOTFS="$WORKDIR/rootfs-broken"
mkdir -p "$BROKEN_ROOTFS"
"$SELF_DIR/../rootfs/assemble.sh" "$BROKEN_ROOTFS" "$BUILD_DIR/init" /bin/false \
  "$BUILD_DIR/haproxy" "$SELF_DIR/../rootfs/base/etc/haproxy/haproxy.cfg" \
  "$BUILD_DIR/selinux/janus.policy"
BROKEN_BUNDLE="$WORKDIR/bundle-broken"
"$SELF_DIR/../image/release/assemble.sh" "$BROKEN_BUNDLE" "$KERNEL" "$BROKEN_ROOTFS"

HAPROXY_BROKEN_ROOTFS="$WORKDIR/rootfs-haproxy-broken"
mkdir -p "$HAPROXY_BROKEN_ROOTFS"
"$SELF_DIR/../rootfs/assemble.sh" "$HAPROXY_BROKEN_ROOTFS" "$BUILD_DIR/init" "$BUILD_DIR/janusd" \
  /bin/false "$SELF_DIR/../rootfs/base/etc/haproxy/haproxy.cfg" \
  "$BUILD_DIR/selinux/janus.policy"
HAPROXY_BROKEN_BUNDLE="$WORKDIR/bundle-haproxy-broken"
"$SELF_DIR/../image/release/assemble.sh" "$HAPROXY_BROKEN_BUNDLE" "$KERNEL" "$HAPROXY_BROKEN_ROOTFS"

# --- inject all three bundles into disk.img's STATE partition, before
# the first boot (see this script's own header comment for why) ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for name in good broken haproxy-broken; do
  bundle_var="${name^^}_BUNDLE"
  bundle_var="${bundle_var//-/_}"
  bundle_dir="${!bundle_var}"
  debugfs -w -R "mkdir upgrade-$name" "$STATE_IMG" >/dev/null 2>&1
  for f in rootfs.squashfs rootfs.verity uki-a.efi uki-b.efi; do
    debugfs -w -R "write $bundle_dir/$f upgrade-$name/$f" "$STATE_IMG" >/dev/null 2>&1
  done
done
dd if="$STATE_IMG" of="$DISK" bs=512 seek="$STATE_START_SECTOR" conv=notrunc status=none
echo "Injected all three release bundles into disk.img's STATE partition"

# --- boot slot A for real ---
boot_disk() {
  local log="$1"
  local ovmf_vars="$WORKDIR/OVMF_VARS-$(basename "$log").fd"
  cp "$OVMF_VARS_TEMPLATE" "$ovmf_vars"

  qemu-system-x86_64 \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$ovmf_vars" \
    -drive file="$DISK",format=raw,if=virtio \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
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

# wait_log waits for a grep -q match to appear in a log file - used
# below for conditions that have no HTTP-visible signal at all (the
# broken slot never answers HTTP; what's being waited for is a log line
# instead).
wait_log() {
  local log="$1" pattern="$2" timeout_secs="$3"
  local deadline=$((SECONDS + timeout_secs))
  while [ "$SECONDS" -lt "$deadline" ]; do
    grep -q "$pattern" "$log" 2>/dev/null && return 0
    sleep 1
  done
  return 1
}

marker_count() {
  grep -c "$MARKER" "$1" 2>/dev/null || echo 0
}

# wait_http_and_marker requires HTTP 200 *and* a minimum boot-marker
# count together in the same poll - same reasoning as hack/qemu-
# lifecycle-upgrade-test.sh's own helper of the same name: :8080 keeps
# answering right up until the 2-second-delayed reboot actually happens
# (scheduleReboot in internal/api/lifecycle.go), so a bare HTTP check
# can pass against the *old*, not-yet-rebooted process - confirmed the
# hard way, this script's own first draft hit exactly that race.
wait_http_and_marker() {
  local port="$1" log="$2" want_markers="$3" timeout_secs="$4" code=""
  local deadline=$((SECONDS + timeout_secs))
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/" || true)"
    if [ "$code" = "200" ] && [ "$(marker_count "$log")" -ge "$want_markers" ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# assert_boot_n checks that the Nth "Kernel command line:" line recorded
# in the console log (the kernel's own canonical, single-per-boot
# restatement of /proc/cmdline) references the expected slot's
# partitions and root hash - same technique hack/qemu-lifecycle-
# upgrade-test.sh's assert_last_boot uses, generalized to a specific
# boot number instead of always the latest, since this script needs to
# check boots #2, #3 and #4 individually against each other, not just
# "whatever the most recent one is".
assert_boot_n() {
  local log="$1" n="$2" data_dev="$3" hash_dev="$4" want_hash="$5" label="$6"
  local line
  line="$(grep "^Kernel command line:" "$log" | sed -n "${n}p")"
  if [ -z "$line" ]; then
    echo "Upgrade health test FAILED: $label - no boot #$n found in the console log" >&2
    echo "--- console output ---" >&2; cat "$log" >&2
    exit 1
  fi
  case "$line" in
    *"verity 1 $data_dev $hash_dev "*) : ;;
    *) echo "Upgrade health test FAILED: $label - boot #$n's cmdline doesn't reference $data_dev/$hash_dev: $line" >&2; exit 1 ;;
  esac
  case "$line" in
    *"$want_hash"*) : ;;
    *) echo "Upgrade health test FAILED: $label - boot #$n's cmdline doesn't carry root hash $want_hash: $line" >&2; exit 1 ;;
  esac
}

LOG="$WORKDIR/console.log"
boot_disk "$LOG"
if ! wait_http "$HOST_PORT_8080" "$HTTP_TIMEOUT_SECS"; then
  echo "Upgrade health test FAILED: slot A never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "Slot A OK: real UEFI boot, HTTP 200 on :8080"

dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Upgrade health test FAILED: couldn't extract pki/$f from STATE" >&2; exit 1; }
done
CTL_ARGS=(-endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key")

# =========================================================================
# Part 1: a healthy upgrade confirms and never reverts.
# =========================================================================
GOOD_SHA256="$(cat "$GOOD_BUNDLE/rootfs.squashfs.sha256")"
GOOD_OUT="$("$CTL" "${CTL_ARGS[@]}" lifecycle upgrade -wait-for-health -health-timeout "$HEALTH_TIMEOUT_SECS" -sha256 "$GOOD_SHA256" /etc/.state/upgrade-good)"
echo "$GOOD_OUT"
if ! echo "$GOOD_OUT" | grep -qi "rebooting"; then
  echo "Upgrade health test FAILED: the 'good' upgrade never reached the 'rebooting' stage" >&2
  exit 1
fi

if ! wait_http_and_marker "$HOST_PORT_8080" "$LOG" 2 "$REBOOT_TIMEOUT_SECS"; then
  echo "Upgrade health test FAILED: no healthy, genuinely-rebooted HTTP 200 after the 'good' upgrade within ${REBOOT_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
GOOD_HASH="$(cat "$GOOD_BUNDLE/rootfs.roothash")"
assert_boot_n "$LOG" 2 /dev/vda4 /dev/vda5 "$GOOD_HASH" "post-'good'-upgrade boot"
echo "Slot B (good) OK: real gRPC Upgrade with wait_for_health installed it and it's live"

# Wait comfortably past HEALTH_TIMEOUT_SECS and confirm it actually got
# confirmed - not just "hasn't reverted yet, but might still" - via
# cmd/janusd's own real HAProxy-level check (internal/
# bootcommit.Confirm), not rootfs/init inferring health from mere
# process survival (that inference doesn't exist any more - see
# rootfs/init/main.go's own comment on why it was removed once this
# real check landed).
if ! wait_log "$LOG" "bootcommit: confirmed healthy" $((HEALTH_TIMEOUT_SECS + 30)); then
  echo "Upgrade health test FAILED: 'bootcommit: confirmed healthy' never appeared within $((HEALTH_TIMEOUT_SECS + 30))s of the 'good' upgrade's reboot" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
if grep -qi "reverting" "$LOG"; then
  echo "Upgrade health test FAILED: a healthy upgrade triggered a revert anyway" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
if [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT_8080}/" || true)" != "200" ]; then
  echo "Upgrade health test FAILED: :8080 stopped answering after confirmation - an unexpected revert?" >&2
  exit 1
fi
echo "Part 1 OK: healthy upgrade confirmed, no revert, still live on slot B"

# =========================================================================
# Part 2: an unhealthy upgrade reverts and reboots back automatically.
# =========================================================================
BROKEN_SHA256="$(cat "$BROKEN_BUNDLE/rootfs.squashfs.sha256")"
BROKEN_OUT="$("$CTL" "${CTL_ARGS[@]}" lifecycle upgrade -wait-for-health -health-timeout "$HEALTH_TIMEOUT_SECS" -sha256 "$BROKEN_SHA256" /etc/.state/upgrade-broken)"
echo "$BROKEN_OUT"
if ! echo "$BROKEN_OUT" | grep -qi "rebooting"; then
  echo "Upgrade health test FAILED: the 'broken' upgrade never reached the 'rebooting' stage" >&2
  exit 1
fi

# The broken slot's janusd (/bin/false) never answers HTTP - wait on
# the boot marker instead to confirm the guest genuinely rebooted into it.
DEADLINE=$((SECONDS + REBOOT_TIMEOUT_SECS))
while [ "$(marker_count "$LOG")" -lt 3 ] && [ "$SECONDS" -lt "$DEADLINE" ]; do sleep 1; done
if [ "$(marker_count "$LOG")" -lt 3 ]; then
  echo "Upgrade health test FAILED: boot marker never appeared a third time - guest didn't reboot into the 'broken' slot" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
BROKEN_HASH="$(cat "$BROKEN_BUNDLE/rootfs.roothash")"
assert_boot_n "$LOG" 3 /dev/vda2 /dev/vda3 "$BROKEN_HASH" "post-'broken'-upgrade boot"
echo "Slot A (broken) OK: real gRPC Upgrade with wait_for_health installed it and the guest rebooted into it"

# Now wait for the *autonomous* revert: a fourth boot marker, HTTP
# healthy again, and the cmdline back on slot B's root hash - all
# without this script ever calling Rollback or Upgrade again.
if ! wait_http_and_marker "$HOST_PORT_8080" "$LOG" 4 $((HEALTH_TIMEOUT_SECS + REBOOT_TIMEOUT_SECS)); then
  echo "Upgrade health test FAILED: no healthy, genuinely-rebooted (4th boot) HTTP 200 after the broken upgrade - automatic revert didn't happen" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
if ! grep -q "giving up and reverting to slot B" "$LOG"; then
  echo "Upgrade health test FAILED: console never logged giving up and reverting to slot B" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
assert_boot_n "$LOG" 4 /dev/vda4 /dev/vda5 "$GOOD_HASH" "post-auto-revert boot"
echo "Part 2 OK: unhealthy upgrade auto-reverted to slot B (not a fixed fallback to slot A) and rebooted, entirely on its own"

# =========================================================================
# Part 3: HAProxy itself (not janusd) never comes up - proves
# cmd/janusd's own real HAProxy-level confirmBootHealth path
# specifically, not just rootfs/init's Supervisor-level backstop Part 2
# already covered.
# =========================================================================
HAPROXY_BROKEN_SHA256="$(cat "$HAPROXY_BROKEN_BUNDLE/rootfs.squashfs.sha256")"
HAPROXY_BROKEN_OUT="$("$CTL" "${CTL_ARGS[@]}" lifecycle upgrade -wait-for-health -health-timeout "$HEALTH_TIMEOUT_SECS" -sha256 "$HAPROXY_BROKEN_SHA256" /etc/.state/upgrade-haproxy-broken)"
echo "$HAPROXY_BROKEN_OUT"
if ! echo "$HAPROXY_BROKEN_OUT" | grep -qi "rebooting"; then
  echo "Upgrade health test FAILED: the 'haproxy-broken' upgrade never reached the 'rebooting' stage" >&2
  exit 1
fi

# janusd itself is the *real* binary here and starts fine - unlike
# Part 2, wait on the boot marker AND janusd's own "confirming
# health" log line, proving this boot's control-plane daemon genuinely
# came up (not just the kernel), before HAProxy's own absence is what
# eventually triggers the revert.
DEADLINE=$((SECONDS + REBOOT_TIMEOUT_SECS))
while { [ "$(marker_count "$LOG")" -lt 5 ] || ! grep -q "bootcommit: confirming health for slot A" "$LOG"; } && [ "$SECONDS" -lt "$DEADLINE" ]; do sleep 1; done
if [ "$(marker_count "$LOG")" -lt 5 ]; then
  echo "Upgrade health test FAILED: boot marker never appeared a fifth time - guest didn't reboot into the 'haproxy-broken' slot" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
if ! grep -q "bootcommit: confirming health for slot A" "$LOG"; then
  echo "Upgrade health test FAILED: janusd never started its own health confirmation for slot A - did janusd itself fail to start too?" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
HAPROXY_BROKEN_HASH="$(cat "$HAPROXY_BROKEN_BUNDLE/rootfs.roothash")"
assert_boot_n "$LOG" 5 /dev/vda2 /dev/vda3 "$HAPROXY_BROKEN_HASH" "post-'haproxy-broken'-upgrade boot"
echo "Slot A (haproxy-broken) OK: janusd itself came up for real and started confirming health"

# The autonomous revert this time comes from cmd/janusd's own
# confirmBootHealth, not rootfs/init's GiveUpAfter - confirm the
# distinguishing log line, not just "some revert happened".
if ! wait_http_and_marker "$HOST_PORT_8080" "$LOG" 6 $((HEALTH_TIMEOUT_SECS + REBOOT_TIMEOUT_SECS)); then
  echo "Upgrade health test FAILED: no healthy, genuinely-rebooted (6th boot) HTTP 200 after the haproxy-broken upgrade - automatic revert didn't happen" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
if ! grep -q "bootcommit: rebooting to complete the revert to slot B" "$LOG"; then
  echo "Upgrade health test FAILED: console never logged cmd/janusd's own revert-to-slot-B line" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
assert_boot_n "$LOG" 6 /dev/vda4 /dev/vda5 "$GOOD_HASH" "post-second-auto-revert boot"
echo "Part 3 OK: HAProxy-level health check (not just janusd process survival) caught a broken HAProxy and reverted+rebooted, entirely on its own"

echo "Upgrade health test OK: wait_for_health confirms a healthy upgrade and stays; reverts+reboots automatically both when janusd itself can't stay up (rootfs/init) and when janusd runs fine but HAProxy never comes up (cmd/janusd's own real health check)"
