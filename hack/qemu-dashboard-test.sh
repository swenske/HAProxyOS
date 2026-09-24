#!/usr/bin/env bash
# Proves the management dashboard's backend (dashboard/backend) works
# end to end against a real, running node - not a mock gRPC server, not
# just that the Go code compiles. Reuses the same real-boot + debugfs
# PKI extraction pattern every other lifecycle test in this project uses
# (see hack/qemu-lifecycle-rollback-test.sh) to get a real admin
# credential for a real booted node, then drives dashboardd's own REST
# API exactly the way the (still-to-come) frontend will:
#
#   1. boot disk.img for real, extract ca.crt/admin.crt/admin.key from
#      its STATE partition via debugfs.
#   2. start dashboardd against a scratch data directory.
#   3. POST /api/nodes with the node's real address + the extracted
#      admin credential as the one-time "bootstrap" credential - proves
#      the add-node flow genuinely calls GenerateClientConfiguration
#      against the real node and gets back a working, freshly-issued
#      service credential (not the bootstrap one relayed - see
#      dashboard/backend/internal/nodeproxy's own package doc for why
#      that's not just a design choice but a cryptographic
#      impossibility).
#   4. GET /api/nodes lists it (name/address/port only, no credential).
#   5. hit the allocated per-node HTTPS port with the *bootstrap*
#      admin cert as the TLS client certificate (any cert issued by the
#      node's own CA satisfies the gate, not specifically the stored
#      service one - the gate and the relay credential are deliberately
#      different keys) and check the JSON response contains real values
#      relayed from the actual node (same values hack/
#      qemu-system-info-test.sh already proves are real).
#   6. confirm the mTLS gate actually rejects both no client cert at all
#      and a cert from an unrelated CA - a real security boundary, not
#      just "it happens to work with the right cert".
#   7. DELETE the node, confirm it's gone from the list AND its port
#      stops accepting connections entirely.
#   8. re-add it, restart dashboardd against the *same* data directory,
#      and confirm both the registry and the per-node listener come back
#      without needing to re-add anything - the whole point of
#      persisting to disk rather than keeping the registry in memory
#      only.
#
# Usage: hack/qemu-dashboard-test.sh <disk.img> <dashboardd-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <dashboardd-bin>}"
DASHBOARDD="${2:?usage: $0 <disk.img> <dashboardd-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_DASHBOARD_HTTP_TIMEOUT:-40}"
HOST_HTTP_PORT="${QEMU_DASHBOARD_NODE_HTTP_PORT:-18120}"
HOST_GRPC_PORT="${QEMU_DASHBOARD_NODE_GRPC_PORT:-18121}"
DASHBOARD_ADDR_PORT="${QEMU_DASHBOARD_ADDR_PORT:-18122}"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
DASHBOARD_PID=""
cleanup() {
  [ -n "$DASHBOARD_PID" ] && kill "$DASHBOARD_PID" 2>/dev/null || true
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# --- boot the real node ---
OVMF_VARS="$WORKDIR/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
LOG="$WORKDIR/console.log"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
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
  echo "Dashboard test FAILED: node never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "Node OK: real UEFI boot, HTTP 200"

# --- extract PKI from STATE (same reasoning as every other lifecycle test) ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Dashboard test FAILED: couldn't extract pki/$f from disk.img's STATE partition" >&2; exit 1; }
done

# --- start dashboardd ---
mkdir -p "$WORKDIR/data"
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -data-dir "$WORKDIR/data" > "$WORKDIR/dashboardd.log" 2>&1 &
DASHBOARD_PID=$!
sleep 1
if ! kill -0 "$DASHBOARD_PID" 2>/dev/null; then
  echo "Dashboard test FAILED: dashboardd exited immediately" >&2
  cat "$WORKDIR/dashboardd.log" >&2
  exit 1
fi

# --- the main SPA (dashboard/frontend, go:embed'd) must actually be
# served, not just the REST API ---
MAIN_UI="$(curl -s "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/")"
echo "$MAIN_UI" | grep -q '<div id="root">' || { echo "Dashboard test FAILED: main SPA index.html not served at /: $MAIN_UI" >&2; exit 1; }
echo "Main SPA OK: dashboard/frontend's built index.html is served at /"

# dashboardd allocates the first free port in its own pool - pin it low
# for this test by patching the request's own expectations rather than
# the binary: read back whatever port it actually picked.
python3 - "$WORKDIR/ca.crt" "$WORKDIR/admin.crt" "$WORKDIR/admin.key" "$HOST_GRPC_PORT" "$WORKDIR/add-node.json" <<'PYEOF'
import json, sys
ca, crt, key, grpc_port, out = sys.argv[1:6]
req = {
    "name": "test-node",
    "address": f"127.0.0.1:{grpc_port}",
    "ca_cert_pem": open(ca).read(),
    "bootstrap_cert_pem": open(crt).read(),
    "bootstrap_key_pem": open(key).read(),
}
open(out, "w").write(json.dumps(req))
PYEOF

ADD_RESP="$(curl -s -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" -H "Content-Type: application/json" -d @"$WORKDIR/add-node.json")"
echo "add-node response: $ADD_RESP"
NODE_ID="$(echo "$ADD_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
NODE_LISTEN_PORT="$(echo "$ADD_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["port"])')"
if [ -z "$NODE_ID" ] || [ -z "$NODE_LISTEN_PORT" ]; then
  echo "Dashboard test FAILED: add-node didn't return an id/port" >&2
  cat "$WORKDIR/dashboardd.log" >&2
  exit 1
fi
echo "Add-node OK: id=$NODE_ID port=$NODE_LISTEN_PORT"

# --- list ---
LIST_RESP="$(curl -s "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
echo "$LIST_RESP" | grep -qF "\"id\":\"$NODE_ID\"" || { echo "Dashboard test FAILED: GET /api/nodes didn't list the registered node: $LIST_RESP" >&2; exit 1; }
if echo "$LIST_RESP" | grep -q "bootstrap\|service"; then
  echo "Dashboard test FAILED: GET /api/nodes leaked credential material: $LIST_RESP" >&2
  exit 1
fi
echo "List OK: node present, no credential material in the response"

# --- per-node relay, with a valid cert from the node's own CA ---
INFO="$(curl -sk --cert "$WORKDIR/admin.crt" --key "$WORKDIR/admin.key" "https://127.0.0.1:${NODE_LISTEN_PORT}/api/info")"
echo "relay response: $INFO"
echo "$INFO" | grep -q '"kernel_version"' || { echo "Dashboard test FAILED: relay response missing kernel_version: $INFO" >&2; exit 1; }
echo "$INFO" | grep -q '"active_slot":"A"' || { echo "Dashboard test FAILED: relay response's active_slot wasn't A: $INFO" >&2; exit 1; }
mem_total="$(echo "$INFO" | python3 -c 'import json,sys; print(json.load(sys.stdin)["memory"]["total_bytes"])')"
[ "$mem_total" -gt 0 ] || { echo "Dashboard test FAILED: relayed memory total_bytes was 0" >&2; exit 1; }
echo "Relay OK: real data (kernel_version, active_slot=A, memory=${mem_total} bytes) genuinely round-tripped through the dashboard to the real node and back"

# --- the per-node view (nodeproxy's own go:embed'd static/index.html,
# not the main SPA) must actually be served, mTLS-gated the same way
# /api/info is ---
NODE_UI="$(curl -sk --cert "$WORKDIR/admin.crt" --key "$WORKDIR/admin.key" "https://127.0.0.1:${NODE_LISTEN_PORT}/")"
echo "$NODE_UI" | grep -q '<title>HAProxyOS Node</title>' || { echo "Dashboard test FAILED: per-node dashboard page not served at /: $NODE_UI" >&2; exit 1; }
echo "Per-node UI OK: nodeproxy's own dashboard page is served at / behind the same mTLS gate as /api/info"

# --- the mTLS gate must reject both no cert and the wrong CA ---
no_cert_code="$(curl -sk -o /dev/null -w '%{http_code}' -m 3 "https://127.0.0.1:${NODE_LISTEN_PORT}/api/info" || true)"
if [ "$no_cert_code" != "000" ]; then
  echo "Dashboard test FAILED: connecting with no client cert should be refused at the TLS handshake, got HTTP $no_cert_code" >&2
  exit 1
fi
openssl req -x509 -newkey ed25519 -keyout "$WORKDIR/wrong.key" -out "$WORKDIR/wrong.crt" -days 1 -nodes -subj "/CN=wrong" >/dev/null 2>&1
wrong_ca_code="$(curl -sk --cert "$WORKDIR/wrong.crt" --key "$WORKDIR/wrong.key" -o /dev/null -w '%{http_code}' -m 3 "https://127.0.0.1:${NODE_LISTEN_PORT}/api/info" || true)"
if [ "$wrong_ca_code" != "000" ]; then
  echo "Dashboard test FAILED: connecting with a cert from an unrelated CA should be refused at the TLS handshake, got HTTP $wrong_ca_code" >&2
  exit 1
fi
echo "mTLS gate OK: both no-cert and wrong-CA connections refused at the TLS handshake itself"

# --- delete ---
del_code="$(curl -s -X DELETE -o /dev/null -w '%{http_code}' "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes/${NODE_ID}")"
[ "$del_code" = "204" ] || { echo "Dashboard test FAILED: DELETE returned $del_code, want 204" >&2; exit 1; }
LIST_AFTER_DELETE="$(curl -s "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
if echo "$LIST_AFTER_DELETE" | grep -qF "\"id\":\"$NODE_ID\""; then
  echo "Dashboard test FAILED: node still listed after DELETE: $LIST_AFTER_DELETE" >&2
  exit 1
fi
after_delete_code="$(curl -sk -o /dev/null -w '%{http_code}' -m 3 --cert "$WORKDIR/admin.crt" --key "$WORKDIR/admin.key" "https://127.0.0.1:${NODE_LISTEN_PORT}/api/info" || true)"
[ "$after_delete_code" = "000" ] || { echo "Dashboard test FAILED: per-node port still answering after DELETE (got $after_delete_code)" >&2; exit 1; }
echo "Delete OK: node unregistered, its listener stopped accepting connections entirely"

# --- persistence across a restart ---
curl -s -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" -H "Content-Type: application/json" -d @"$WORKDIR/add-node.json" > /dev/null
kill "$DASHBOARD_PID"
wait "$DASHBOARD_PID" 2>/dev/null || true
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -data-dir "$WORKDIR/data" > "$WORKDIR/dashboardd-restart.log" 2>&1 &
DASHBOARD_PID=$!
sleep 1
RESTART_LIST="$(curl -s "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
echo "$RESTART_LIST" | grep -q '"name":"test-node"' || { echo "Dashboard test FAILED: node registry didn't survive a restart: $RESTART_LIST" >&2; cat "$WORKDIR/dashboardd-restart.log" >&2; exit 1; }
RESTART_PORT="$(echo "$RESTART_LIST" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["port"])')"
restart_relay_code="$(curl -sk -o /dev/null -w '%{http_code}' --cert "$WORKDIR/admin.crt" --key "$WORKDIR/admin.key" "https://127.0.0.1:${RESTART_PORT}/api/info" || true)"
[ "$restart_relay_code" = "200" ] || { echo "Dashboard test FAILED: per-node listener didn't come back after restart (got $restart_relay_code)" >&2; exit 1; }
echo "Restart persistence OK: node registry and per-node listener both survived a dashboardd restart against the same data directory"

echo "Dashboard test OK: add/list/relay/mTLS-gate/delete/restart-persistence all verified against a real running node"
