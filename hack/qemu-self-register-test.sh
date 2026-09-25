#!/usr/bin/env bash
# Proves Point 2 suite tranche 5 end to end: a node provisioned with a
# Controller (LifecycleService.Install's controller_address/
# controller_ca_cert, tranche 4) actually self-registers with a real,
# running Janus Controller (dashboardd) on its own first boot - not a
# mock HTTP server standing in for either side.
#
#   1. start dashboardd natively on the test host (-advertise-address
#      10.0.2.2 - QEMU usermode networking's own fixed host-gateway
#      address, the address a QEMU guest actually uses to reach a
#      service on its host; without this, dashboardd's TLS identity
#      certificate's SAN list - built from this process's own network
#      interfaces, see loadOrCreateDashboardIdentity - would never
#      include an address the guest could verify the handshake
#      against, and every self-registration attempt below would fail
#      TLS verification outright, a real gap found while building this
#      test, not assumed - see dashboard/backend/main.go's own comment
#      on -advertise-address and dashboard/README.md's matching note
#      for the identical real-world case, a Docker bridge-networked
#      deployment).
#   2. set up the dashboard's one-time admin auth (same cookie-jar
#      pattern hack/qemu-dashboard-test.sh uses) so /api/pending and
#      /api/nodes are reachable.
#   3. run janusd natively (root, for CAP_SYS_CHROOT - same pattern
#      hack/lifecycle-install-test.sh uses) purely to serve the Install
#      RPC against a blank disk file, passing -controller-address
#      10.0.2.2:<register-port> and -controller-ca <dashboardd's own
#      identity cert> - a genuinely different disk than the one this
#      native janusd itself would ever boot from.
#   4. boot the installed disk for real under OVMF, virtio-net +
#      usermode NAT (the guest reaches 10.0.2.2 automatically - no
#      special hostfwd needed for this, guest-initiated connections to
#      the host "just work" under QEMU's slirp stack) - and poll the
#      *real* dashboardd's GET /api/pending until the node shows up on
#      its own, with no RPC call from this script driving it: proves
#      cmd/janusd's own selfRegisterIfConfigured actually ran at boot,
#      read the provisioned config back off STATE (rootfs/init's
#      mountState bind-mounting STATE's controller/ subdirectory),
#      minted a fresh service credential locally, and reached the real
#      Controller over TLS verified against the CA it was given at
#      provisioning time.
#   5. approve the pending entry through the real dashboard API, then
#      confirm the resulting per-node listener's mTLS gate trusts the
#      CA the node self-reported - a cert extracted from the booted
#      guest's own STATE partition completes the handshake, a cert from
#      an unrelated CA is refused at the handshake itself. Doesn't go
#      all the way to a relayed response with real data the way hack/
#      qemu-dashboard-test.sh's own relay check does - see this part's
#      own comment in the script for why (QEMU usermode networking's
#      asymmetry: the host has no route back to the guest's own
#      advertised address, only hack/qemu-dashboard-test.sh's real,
#      host-reachable node address works for that, and nodeproxy's
#      relay logic itself is unchanged by this tranche).
#   6. boot the *same* disk.img a second time (a fresh QEMU invocation,
#      not a live in-guest reboot - nothing here needs the RPC-driven
#      reboot machinery hack/qemu-lifecycle-rollback-test.sh exercises)
#      and confirm no second pending entry ever appears - proving
#      internal/selfregister's persisted "registered" marker (on
#      STATE, survives the reboot) actually stops the node from
#      re-announcing itself every boot.
#
# Usage: hack/qemu-self-register-test.sh <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

ROOTFS_DIR="${1:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
KERNEL="${2:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
HAPROXY_BIN="${3:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
JANUSD_BIN="${4:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
CTL_REL="${5:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
DASHBOARDD_REL="${6:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
CTL="$(cd "$(dirname "$CTL_REL")" && pwd)/$(basename "$CTL_REL")"
DASHBOARDD="$(cd "$(dirname "$DASHBOARDD_REL")" && pwd)/$(basename "$DASHBOARDD_REL")"

DISK_MB="${SELFREG_TEST_DISK_MB:-512}"
HTTP_TIMEOUT_SECS="${SELFREG_TEST_HTTP_TIMEOUT:-40}"
REGISTER_TIMEOUT_SECS="${SELFREG_TEST_REGISTER_TIMEOUT:-30}"
HOST_HTTP_PORT="${SELFREG_TEST_HOST_HTTP_PORT:-18140}"
HOST_GRPC_PORT="${SELFREG_TEST_HOST_GRPC_PORT:-18141}"
NATIVE_GRPC_PORT="${SELFREG_TEST_NATIVE_GRPC_PORT:-19510}"
DASHBOARD_ADDR_PORT="${SELFREG_TEST_DASHBOARD_ADDR_PORT:-18142}"
DASHBOARD_REGISTER_PORT="${SELFREG_TEST_DASHBOARD_REGISTER_PORT:-18143}"
QEMU_HOST_GATEWAY="10.0.2.2" # QEMU usermode/slirp's fixed host-gateway address, reachable from any guest without extra config

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
JANUSD_PID=""
DASHBOARD_PID=""
QEMU_PID=""
# Same reasoning as hack/lifecycle-install-test.sh's own cleanup trap:
# kill native processes by pid, never by a fuzzy `pkill -f` command-line
# match (this script's own argv contains strings like "build/haproxy"
# that would otherwise match and kill the test script's own process).
kill_native_haproxy() {
  sudo pkill -x haproxy 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    pgrep -x haproxy >/dev/null 2>&1 || return 0
    sleep 0.5
  done
  sudo pkill -x -9 haproxy 2>/dev/null || true
}
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  [ -n "$JANUSD_PID" ] && sudo kill "$JANUSD_PID" 2>/dev/null || true
  kill_native_haproxy
  [ -n "$DASHBOARD_PID" ] && kill "$DASHBOARD_PID" 2>/dev/null || true
  sudo rm -rf "$WORKDIR"
}
trap cleanup EXIT

# =========================================================================
# Part 1: a real dashboardd, natively, with the host-gateway address
# baked into its TLS identity SAN list.
# =========================================================================
mkdir -p "$WORKDIR/dashboard-data"
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" \
  -data-dir "$WORKDIR/dashboard-data" -advertise-address "$QEMU_HOST_GATEWAY" \
  > "$WORKDIR/dashboardd.log" 2>&1 &
DASHBOARD_PID=$!
sleep 1
if ! kill -0 "$DASHBOARD_PID" 2>/dev/null; then
  echo "Self-register test FAILED: dashboardd exited immediately" >&2
  cat "$WORKDIR/dashboardd.log" >&2
  exit 1
fi

COOKIE_JAR="$WORKDIR/cookies.txt"
setup_code="$(curl -s -c "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/setup" -H "Content-Type: application/json" -d '{"password":"self-register-test-admin-pw"}')"
[ "$setup_code" = "204" ] || { echo "Self-register test FAILED: dashboard admin setup returned $setup_code, want 204" >&2; exit 1; }
echo "Part 1 OK: dashboardd running natively, admin auth established"

CONTROLLER_CA="$WORKDIR/dashboard-data/dashboard-identity.crt"
[ -s "$CONTROLLER_CA" ] || { echo "Self-register test FAILED: dashboardd never wrote its own identity cert to $CONTROLLER_CA" >&2; exit 1; }

# =========================================================================
# Part 2: build a release bundle and Install it onto a blank disk,
# provisioned with the Controller above - same native-janusd-serving-
# Install pattern hack/lifecycle-install-test.sh uses.
# =========================================================================
BUNDLE="$WORKDIR/bundle"
"$SELF_DIR/../image/release/assemble.sh" "$BUNDLE" "$KERNEL" "$ROOTFS_DIR"
SHA256="$(cat "$BUNDLE/rootfs.squashfs.sha256")"

PKI_DIR="$WORKDIR/pki"
sudo mkdir -p /run/janus /etc/haproxy /etc/janus
sudo cp "$SELF_DIR/../rootfs/base/etc/haproxy/haproxy.cfg" /etc/haproxy/haproxy.cfg
sudo mkdir -p /var/empty && sudo chmod 000 /var/empty

sudo "$JANUSD_BIN" -addr ":$NATIVE_GRPC_PORT" -haproxy-binary "$HAPROXY_BIN" -pki-dir "$PKI_DIR" \
  > "$WORKDIR/native.log" 2>&1 &
JANUSD_PID=$!

DEADLINE=$((SECONDS + 20))
while ! sudo test -s "$PKI_DIR/admin.crt" && [ "$SECONDS" -lt "$DEADLINE" ]; do sleep 1; done
if ! sudo test -s "$PKI_DIR/admin.crt"; then
  echo "Self-register test FAILED: janusd never bootstrapped PKI at $PKI_DIR within 20s" >&2
  cat "$WORKDIR/native.log" >&2
  exit 1
fi
NATIVE_CTL_ARGS=(-endpoint "127.0.0.1:${NATIVE_GRPC_PORT}" -ca "$PKI_DIR/ca.crt" -cert "$PKI_DIR/admin.crt" -key "$PKI_DIR/admin.key")

BLANK_DISK="$WORKDIR/blank-disk.img"
truncate -s "${DISK_MB}M" "$BLANK_DISK"

INSTALL_OUT="$(sudo "$CTL" "${NATIVE_CTL_ARGS[@]}" lifecycle install -sha256 "$SHA256" \
  -controller-address "${QEMU_HOST_GATEWAY}:${DASHBOARD_REGISTER_PORT}" -controller-ca "$CONTROLLER_CA" \
  "$BLANK_DISK" "$BUNDLE")"
echo "$INSTALL_OUT"
if ! echo "$INSTALL_OUT" | grep -qi '\[done '; then
  echo "Self-register test FAILED: Install never reached the 'done' stage" >&2
  exit 1
fi
sudo chown "$(id -u):$(id -g)" "$BLANK_DISK"

sudo kill "$JANUSD_PID" 2>/dev/null || true
wait "$JANUSD_PID" 2>/dev/null || true
JANUSD_PID=""
kill_native_haproxy
echo "Part 2 OK: Install wrote a Controller-provisioned image"

# =========================================================================
# Part 3: boot the installed disk for real - self-registration is
# expected to happen entirely on its own, no RPC call from this script.
# =========================================================================
LOG="$WORKDIR/console.log"
OVMF_VARS="$WORKDIR/vars.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$BLANK_DISK",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_HTTP_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

DEADLINE=$((SECONDS + HTTP_TIMEOUT_SECS))
CODE=""
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  CODE="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)"
  [ "$CODE" = "200" ] && break
  sleep 1
done
if [ "$CODE" != "200" ]; then
  echo "Self-register test FAILED: the installed disk never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "Part 3 OK: the Controller-provisioned disk boots for real"

# --- poll the real dashboardd's own pending queue - no RPC call here
# drives the registration, only cmd/janusd's own background attempt ---
PENDING_JSON=""
DEADLINE=$((SECONDS + REGISTER_TIMEOUT_SECS))
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  PENDING_JSON="$(curl -s -b "$COOKIE_JAR" "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending")"
  [ "$PENDING_JSON" != "null" ] && [ -n "$PENDING_JSON" ] && break
  sleep 1
done
if [ "$PENDING_JSON" = "null" ] || [ -z "$PENDING_JSON" ]; then
  echo "Self-register test FAILED: no self-registration ever showed up in GET /api/pending within ${REGISTER_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  echo "--- dashboardd log ---" >&2; cat "$WORKDIR/dashboardd.log" >&2
  exit 1
fi
grep -q "selfregister: successfully announced" "$LOG" || { echo "Self-register test FAILED: guest console never logged a successful self-registration" >&2; cat "$LOG" >&2; exit 1; }

PENDING_ID="$(echo "$PENDING_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')"
PENDING_NAME="$(echo "$PENDING_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["name"])')"
[ -n "$PENDING_ID" ] && [ -n "$PENDING_NAME" ] || { echo "Self-register test FAILED: pending entry missing id/name: $PENDING_JSON" >&2; exit 1; }
echo "Part 4 OK: node self-registered on its own - id=$PENDING_ID name=$PENDING_NAME, entry: $PENDING_JSON"

# =========================================================================
# Part 5: approve it through the real API, then confirm the resulting
# per-node listener's mTLS gate genuinely trusts the CA the node
# self-reported (not some other CA it happens to already know about).
#
# This can't go all the way to a real relayed response the way hack/
# qemu-dashboard-test.sh's own relay check does: dashboardd here runs
# natively on the test host, and QEMU's usermode/slirp networking is
# asymmetric - the guest can dial *out* to the host via the fixed
# 10.0.2.2 gateway (which is what made self-registration itself work
# above), but the host has no route *back* to the guest's own internal
# address (10.0.2.15) at all, only to whatever port an explicit
# -netdev hostfwd rule maps - which the guest has no way to know about
# or advertise, and shouldn't need to: on any real network, the
# advertise address self-registration computes is one the Controller
# can actually dial, this asymmetry is purely a QEMU test-harness
# artifact. nodeproxy's relay logic itself is completely unchanged by
# this tranche and is already fully proven end to end against a real,
# host-reachable node address by hack/qemu-dashboard-test.sh - what's
# actually new here, and what this part verifies instead, is that
# approvePending correctly plumbed the node's *self-reported* CA into
# the listener it started.
# =========================================================================
APPROVE_RESP="$(curl -s -b "$COOKIE_JAR" -w '\n%{http_code}' -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending/${PENDING_ID}/approve")"
APPROVE_CODE="$(echo "$APPROVE_RESP" | tail -1)"
APPROVE_BODY="$(echo "$APPROVE_RESP" | sed '$d')"
[ "$APPROVE_CODE" = "201" ] || { echo "Self-register test FAILED: approve returned $APPROVE_CODE: $APPROVE_BODY" >&2; exit 1; }
APPROVED_PORT="$(echo "$APPROVE_BODY" | python3 -c 'import json,sys; print(json.load(sys.stdin)["port"])')"
[ -n "$APPROVED_PORT" ] || { echo "Self-register test FAILED: approve response missing a port: $APPROVE_BODY" >&2; exit 1; }

# Extract a cert from the booted guest's own CA (partition 6, STATE -
# same debugfs extraction every other lifecycle test in this project
# uses) - any cert issued by that CA must satisfy the per-node
# listener's mTLS gate.
STATE_START_SECTOR="$(sgdisk -i 6 "$BLANK_DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$BLANK_DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$BLANK_DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Self-register test FAILED: couldn't extract pki/$f from the booted guest's STATE partition" >&2; exit 1; }
done

# A cert from the guest's own (self-reported) CA must complete the TLS
# handshake and reach the relay handler - proven here by a real 502
# ("Version: ... DeadlineExceeded", from nodeproxy.go's handleInfo
# trying and failing to reach the guest's own unreachable-from-here
# advertise address, per this part's own header comment) rather than a
# handshake-level refusal. handleInfo's own dial uses a 10s context
# timeout (see nodeproxy.go), so this needs real room past that: a
# first draft used a tight 5s client-side timeout here, which made a
# genuinely successful handshake indistinguishable from a real
# refusal, since curl's %{http_code} reports "000" for both "never
# connected" and "connected fine but the response didn't arrive in
# time" - caught with curl -v, which showed the handshake completing
# in full while the tight timeout still reported 000.
right_ca_code="$(curl -sk --cert "$WORKDIR/admin.crt" --key "$WORKDIR/admin.key" -o /dev/null -w '%{http_code}' -m 20 "https://127.0.0.1:${APPROVED_PORT}/api/info" || true)"
if [ "$right_ca_code" != "502" ]; then
  echo "Self-register test FAILED: expected the approved node's listener to accept the right-CA cert and reach the relay handler (502, dial timeout to the unreachable-from-here address), got $right_ca_code" >&2
  exit 1
fi

# A cert from an unrelated CA must be refused at the handshake itself -
# proving the gate is checking against the self-reported CA
# specifically, not just accepting anything.
openssl req -x509 -newkey ed25519 -keyout "$WORKDIR/wrong.key" -out "$WORKDIR/wrong.crt" -days 1 -nodes -subj "/CN=wrong" >/dev/null 2>&1
wrong_ca_code="$(curl -sk --cert "$WORKDIR/wrong.crt" --key "$WORKDIR/wrong.key" -o /dev/null -w '%{http_code}' -m 5 "https://127.0.0.1:${APPROVED_PORT}/api/info" || true)"
if [ "$wrong_ca_code" != "000" ]; then
  echo "Self-register test FAILED: the approved node's listener accepted a cert from an unrelated CA (code $wrong_ca_code), want a handshake-level refusal" >&2
  exit 1
fi
echo "Part 5 OK: approved node's listener gate genuinely trusts the CA the node self-reported (right CA: HTTP $right_ca_code, unrelated CA: refused at the handshake)"

kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

# =========================================================================
# Part 6: boot the same disk a second time - the persisted "registered"
# marker must stop it from ever announcing itself again.
# =========================================================================
LOG2="$WORKDIR/console2.log"
OVMF_VARS2="$WORKDIR/vars2.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS2"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS2" \
  -drive file="$BLANK_DISK",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_HTTP_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG2" \
  &
QEMU_PID=$!

DEADLINE=$((SECONDS + HTTP_TIMEOUT_SECS))
CODE=""
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  CODE="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)"
  [ "$CODE" = "200" ] && break
  sleep 1
done
if [ "$CODE" != "200" ]; then
  echo "Self-register test FAILED: the disk's second boot never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG2" >&2
  exit 1
fi
# Give any (incorrect) second self-registration attempt a few seconds to
# show up, the same way it did the first time, before declaring this
# clean.
sleep 5
if grep -q "selfregister: successfully announced\|selfregister: registration failed" "$LOG2"; then
  echo "Self-register test FAILED: the node attempted to self-register again on its second boot despite the persisted marker" >&2
  cat "$LOG2" >&2
  exit 1
fi
PENDING_AFTER_REBOOT="$(curl -s -b "$COOKIE_JAR" "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending")"
if [ "$PENDING_AFTER_REBOOT" != "null" ] && [ -n "$PENDING_AFTER_REBOOT" ]; then
  echo "Self-register test FAILED: a second pending entry appeared after the second boot: $PENDING_AFTER_REBOOT" >&2
  exit 1
fi
echo "Part 6 OK: the persisted registered marker survived the reboot - no second announcement, no new pending entry"

kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

echo "Self-register test OK: a node provisioned with a Controller at Install time genuinely self-registered on first boot, was approved through the real API, the approved listener's mTLS gate trusted the self-reported CA, and it never announced itself again on a second boot"
