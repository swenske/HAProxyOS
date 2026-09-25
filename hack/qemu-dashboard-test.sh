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
#   3b. tranche 6: add-node via a .pfx upload instead of pasted PEM
#      text (dashboard/backend/main.go's parseAddNodeRequest, the
#      frontend's default mode as of dashboard/frontend/src/App.jsx's
#      AddNodeForm) - a real openssl-built .pfx bundling the admin
#      cert+key plus the node's ca.crt (via -certfile, exactly how a
#      real user's would be built), added and immediately removed
#      again so it doesn't interfere with the main JSON-based flow
#      below. Also checks the two real error paths: a wrong password,
#      and a .pfx missing the bundled CA certificate - both need a
#      real, actionable message, not a generic parse failure.
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
#   7. exercise the tranche 4 ops/config relay (dashboard/backend/
#      internal/nodeproxy/ops.go) against the node's own bootstrap
#      config - deliberately scoped to what's testable without a custom
#      boot fixture: ShowInfo and GetConfig against real values;
#      ApplyConfig with the exact same config read back (must be
#      accepted - proves the round trip) and with deliberately garbage
#      config (must be rejected, with a real validation error, and must
#      NOT disturb the running config - internal/haproxy.Manager.Apply's
#      own "never overwritten on a rejected apply" guarantee, now
#      proven through the dashboard's own relay too); BackendList
#      against its own real not-yet-implemented state (internal/api/
#      haproxy.go's own doc comment - falls through to
#      UnimplementedHAProxyServiceServer, so the relay must surface a
#      real error here, not fake a success); MapList/CertificateList
#      against the bootstrap config's genuinely empty map/cert list (a
#      correctly-relayed empty response, not a gap - protojson's
#      omitempty drops an empty repeated field entirely, confirmed
#      against a real boot before encoding this assertion); a real
#      CertificateUpload/List/Delete round trip (HAProxy's cert *store*
#      accepts uploads unconditionally, unlike file-backed maps/ACLs,
#      which only exist if the config already declares one - exercising
#      a real MapUpdate/ACLUpdate success case would need injecting a
#      whole custom haproxy.cfg + map/ACL files onto STATE before boot,
#      real effort for coverage `internal/haproxy`'s own CI step already
#      provides at the non-dashboard layer - so this test settles for
#      proving MapUpdate genuinely reaches the real node and surfaces
#      its real "no such map" error correctly, not a staged success).
#      Writing this step's assertions found two real, previously-
#      undiscovered bugs, both fixed alongside this test: (a) a missing
#      SELinux policy rule (selinux/policy.conf) denied the haproxy_t
#      domain write access to the fifo_file pipe internal/haproxy.
#      Manager.Validate's `haproxy -c` inherits from its janusd_t
#      parent (a genuinely different object from the supervised
#      process's own console-backed stdout/stderr, already covered) -
#      every validation error was being silently denied mid-write,
#      captured as an empty error list instead of haproxy's real
#      parse-error text, on a real boot only (a permissive/plain local
#      run never saw it - see manager.go's own comment); (b) internal/
#      haproxy.Manager.statsCommand had no read deadline at all - a
#      malformed multi-line payload (found via a test-harness bug of
#      this test's own early draft, not production code, but the gap
#      it exposed is real) leaves HAProxy's stats socket waiting for
#      more input forever, blocking the reading goroutine past any
#      caller's own timeout (fixed with a bounded conn.SetDeadline).
#   8. DELETE the node, confirm it's gone from the list AND its port
#      stops accepting connections entirely.
#   9. re-add it, restart dashboardd against the *same* data directory,
#      and confirm both the registry and the per-node listener come back
#      without needing to re-add anything - the whole point of
#      persisting to disk rather than keeping the registry in memory
#      only.
#   10. tranche 7: the main port's own admin auth (dashboard/backend/
#      internal/auth) - confirmed via a real cookie jar throughout this
#      whole script (every /api/nodes* call needs it, matching what the
#      real UI now enforces): /api/nodes with no session at all must be
#      refused (401), a first-run setup call establishes the one admin
#      password and logs the caller in via the returned session cookie,
#      and - after the dashboardd restart in step 9, since sessions are
#      deliberately in-memory-only and don't survive a restart - a fresh
#      login call is needed before the registry is reachable again.
#   11. Point 2 suite, tranche 2: node self-registration (dashboard/
#      backend/internal/pending + register.go's startRegistrationListener)
#      - a synthetic node CA + service credential (standing in for what
#      a real janusd would generate locally and send - tranche 2 is
#      the Controller side only, janusd doesn't self-register yet)
#      POSTed to the dedicated TLS registration port (separate from
#      both :8080 and the per-node pool - see startRegistrationListener's
#      own doc comment for why). Checks: a real registration is
#      accepted and shows up in GET /api/pending (auth-gated, like
#      everything else under /api/); missing required fields refused
#      with 400; a cert/key pair that doesn't actually parse together
#      refused with 400 and a real error, not a generic failure; the
#      pending entry survives a dashboardd restart (same reasoning as
#      the node registry itself - a node only announces once per boot,
#      so losing this to an in-memory store would strand it).
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
DASHBOARD_REGISTER_PORT="${QEMU_DASHBOARD_REGISTER_PORT:-18123}"

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
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" -data-dir "$WORKDIR/data" > "$WORKDIR/dashboardd.log" 2>&1 &
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

# --- tranche 7: admin auth gates everything under /api/nodes* now -
# confirm the gate itself before doing anything it would block ---
COOKIE_JAR="$WORKDIR/cookies.txt"
unauth_code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
[ "$unauth_code" = "401" ] || { echo "Dashboard test FAILED: /api/nodes with no session should be 401, got $unauth_code" >&2; exit 1; }

AUTH_STATUS="$(curl -s "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/status")"
echo "$AUTH_STATUS" | grep -q '"setup_required":true' || { echo "Dashboard test FAILED: a fresh data dir should report setup_required, got: $AUTH_STATUS" >&2; exit 1; }

short_pw_code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/setup" -H "Content-Type: application/json" -d '{"password":"short"}')"
[ "$short_pw_code" = "400" ] || { echo "Dashboard test FAILED: a too-short setup password should be refused with 400, got $short_pw_code" >&2; exit 1; }

setup_code="$(curl -s -c "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/setup" -H "Content-Type: application/json" -d '{"password":"dashboard-test-admin-pw"}')"
[ "$setup_code" = "204" ] || { echo "Dashboard test FAILED: first-run setup should return 204, got $setup_code" >&2; exit 1; }

second_setup_code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/setup" -H "Content-Type: application/json" -d '{"password":"dashboard-test-admin-pw"}')"
[ "$second_setup_code" = "400" ] || { echo "Dashboard test FAILED: a second setup call should be refused, got $second_setup_code" >&2; exit 1; }
echo "Auth OK: /api/nodes refused with no session, setup validated (short password, second-setup refusal), session cookie established"

# --- Point 2 suite, tranche 2: node self-registration (dashboard/backend/
# internal/pending + register.go) - a synthetic node CA + service
# credential standing in for what a real janusd would generate locally
# and send (janusd itself doesn't self-register yet - this tranche is
# the Controller side only) ---
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout "$WORKDIR/reg-node-ca.key" -out "$WORKDIR/reg-node-ca.crt" -days 1 -nodes -subj "/CN=synthetic node CA" >/dev/null 2>&1
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout "$WORKDIR/reg-service.key" -out "$WORKDIR/reg-service.csr" -nodes -subj "/CN=service" >/dev/null 2>&1
openssl x509 -req -in "$WORKDIR/reg-service.csr" -CA "$WORKDIR/reg-node-ca.crt" -CAkey "$WORKDIR/reg-node-ca.key" \
  -CAcreateserial -out "$WORKDIR/reg-service.crt" -days 1 >/dev/null 2>&1
python3 -c '
import json, sys
ca, crt, key, out = sys.argv[1:5]
req = {
    "name": "self-registered-test-node",
    "address": "10.0.0.7:9505",
    "ca_cert_pem": open(ca).read(),
    "service_cert_pem": open(crt).read(),
    "service_key_pem": open(key).read(),
}
open(out, "w").write(json.dumps(req))
' "$WORKDIR/reg-node-ca.crt" "$WORKDIR/reg-service.crt" "$WORKDIR/reg-service.key" "$WORKDIR/register.json"

REGISTER_RESP="$(curl -sk -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d @"$WORKDIR/register.json")"
PENDING_ID="$(echo "$REGISTER_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)"
[ -n "$PENDING_ID" ] || { echo "Dashboard test FAILED: registration didn't return an id: $REGISTER_RESP" >&2; exit 1; }

PENDING_LIST="$(curl -s -b "$COOKIE_JAR" "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending")"
echo "$PENDING_LIST" | grep -qF "\"id\":\"$PENDING_ID\"" || { echo "Dashboard test FAILED: GET /api/pending didn't list the self-registered node: $PENDING_LIST" >&2; exit 1; }
echo "$PENDING_LIST" | grep -q '"name":"self-registered-test-node"' || { echo "Dashboard test FAILED: pending entry missing its name: $PENDING_LIST" >&2; exit 1; }
echo "Node self-registration OK: $REGISTER_RESP, listed in GET /api/pending"

missing_fields_code="$(curl -sk -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d '{"name":"incomplete"}')"
[ "$missing_fields_code" = "400" ] || { echo "Dashboard test FAILED: registration with missing fields should be 400, got $missing_fields_code" >&2; exit 1; }

python3 -c '
import json, sys
ca, crt, out = sys.argv[1:4]
req = {"name": "bad-key", "address": "10.0.0.8:9505", "ca_cert_pem": open(ca).read(), "service_cert_pem": open(crt).read(), "service_key_pem": "not a real key"}
open(out, "w").write(json.dumps(req))
' "$WORKDIR/reg-node-ca.crt" "$WORKDIR/reg-service.crt" "$WORKDIR/register-bad-key.json"
bad_key_resp="$(curl -sk -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d @"$WORKDIR/register-bad-key.json" -w '\n%{http_code}')"
bad_key_code="$(echo "$bad_key_resp" | tail -1)"
[ "$bad_key_code" = "400" ] || { echo "Dashboard test FAILED: registration with a malformed key should be 400, got $bad_key_code: $bad_key_resp" >&2; exit 1; }
echo "$bad_key_resp" | grep -q "don't form a valid certificate" || { echo "Dashboard test FAILED: malformed key should get a real, actionable error, got: $bad_key_resp" >&2; exit 1; }
echo "Registration error paths OK: missing fields and a malformed cert/key pair both refused with a real error"

# --- add-node via .pfx upload (the frontend's default mode as of
# tranche 6 - see dashboard/backend/main.go's parseAddNodeRequest and
# dashboard/frontend/src/App.jsx's AddNodeForm) - a real .pfx built the
# same way a user's would be (openssl pkcs12 -export -certfile ca.crt,
# so the CA rides along inside the bundle, not just the leaf cert),
# added and immediately removed again so it doesn't interfere with the
# main JSON-based add-node flow below (still the primary path this
# script exercises end to end - the .pfx path only needs proving it
# reaches the same parseAddNodeRequest outcome, not a second full
# relay/mTLS-gate/restart-persistence pass). Also checks the two real
# error paths: a wrong password, and a .pfx missing the bundled CA.
openssl pkcs12 -export -inkey "$WORKDIR/admin.key" -in "$WORKDIR/admin.crt" \
  -certfile "$WORKDIR/ca.crt" -out "$WORKDIR/admin.pfx" -passout pass:dashboard-test-pfx >/dev/null 2>&1
PFX_ADD_RESP="$(curl -s -b "$COOKIE_JAR" -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" \
  -F "name=pfx-test-node" -F "address=127.0.0.1:${HOST_GRPC_PORT}" \
  -F "pfx_password=dashboard-test-pfx" -F "pfx=@$WORKDIR/admin.pfx;type=application/x-pkcs12")"
PFX_NODE_ID="$(echo "$PFX_ADD_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)"
[ -n "$PFX_NODE_ID" ] || { echo "Dashboard test FAILED: .pfx add-node didn't return an id: $PFX_ADD_RESP" >&2; exit 1; }
curl -s -b "$COOKIE_JAR" -X DELETE "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes/${PFX_NODE_ID}" >/dev/null
echo "Add-node via .pfx OK: $PFX_ADD_RESP"

WRONG_PW_RESP="$(curl -s -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" \
  -F "name=wrong-pw" -F "address=127.0.0.1:${HOST_GRPC_PORT}" \
  -F "pfx_password=not-the-password" -F "pfx=@$WORKDIR/admin.pfx;type=application/x-pkcs12")"
[ "$WRONG_PW_RESP" = "400" ] || { echo "Dashboard test FAILED: wrong .pfx password should return 400, got $WRONG_PW_RESP" >&2; exit 1; }

openssl pkcs12 -export -inkey "$WORKDIR/admin.key" -in "$WORKDIR/admin.crt" \
  -out "$WORKDIR/admin-no-ca.pfx" -passout pass:dashboard-test-pfx >/dev/null 2>&1
NO_CA_RESP="$(curl -s -b "$COOKIE_JAR" -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" \
  -F "name=no-ca" -F "address=127.0.0.1:${HOST_GRPC_PORT}" \
  -F "pfx_password=dashboard-test-pfx" -F "pfx=@$WORKDIR/admin-no-ca.pfx;type=application/x-pkcs12")"
echo "$NO_CA_RESP" | grep -q "no CA certificate bundled" || { echo "Dashboard test FAILED: .pfx with no bundled CA should be refused with a clear message, got: $NO_CA_RESP" >&2; exit 1; }
echo ".pfx error paths OK: wrong password and missing-CA both refused with a real, actionable message"

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

ADD_RESP="$(curl -s -b "$COOKIE_JAR" -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" -H "Content-Type: application/json" -d @"$WORKDIR/add-node.json")"
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
LIST_RESP="$(curl -s -b "$COOKIE_JAR" "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
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
echo "$NODE_UI" | grep -q '<title>Janus Node</title>' || { echo "Dashboard test FAILED: per-node dashboard page not served at /: $NODE_UI" >&2; exit 1; }
echo "Per-node UI OK: nodeproxy's own dashboard page is served at / behind the same mTLS gate as /api/info"

# --- tranche 4: ops/config relay (dashboard/backend/internal/nodeproxy/ops.go) ---
DASH_CERT=(--cert "$WORKDIR/admin.crt" --key "$WORKDIR/admin.key")
NODE_BASE="https://127.0.0.1:${NODE_LISTEN_PORT}"

HAPROXY_INFO="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/info")"
echo "$HAPROXY_INFO" | grep -q '"version"' || { echo "Dashboard test FAILED: /api/haproxy/info missing version: $HAPROXY_INFO" >&2; exit 1; }
echo "ShowInfo relay OK: $HAPROXY_INFO"

GETCFG="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/config")"
ORIG_SHA256="$(echo "$GETCFG" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha256"])')"
[ -n "$ORIG_SHA256" ] || { echo "Dashboard test FAILED: /api/haproxy/config missing sha256: $GETCFG" >&2; exit 1; }
echo "GetConfig relay OK: sha256=$ORIG_SHA256"

# Re-apply the exact same config read back - must be accepted (proves the
# round trip byte-for-byte). $(...) capturing GETCFG above only strips
# trailing newlines from the outer JSON document, not the config text's
# own embedded newlines (those survive as escaped "\n" inside the JSON
# string) - safe to re-extract from $GETCFG directly here. Re-encoding
# the raw config bytes through a shell variable instead, and stripping
# ITS trailing newline via $(...), is the mistake that was actually made
# once while writing this test - not this.
python3 -c '
import json, sys
cfg = json.loads(sys.argv[1])["config"]
json.dump({"config": cfg}, open(sys.argv[2], "w"))
' "$GETCFG" "$WORKDIR/apply-same.json"
APPLY_SAME="$(curl -sk "${DASH_CERT[@]}" -X POST -H "Content-Type: application/json" -d @"$WORKDIR/apply-same.json" "${NODE_BASE}/api/haproxy/config")"
echo "$APPLY_SAME" | grep -q '"accepted":true' || { echo "Dashboard test FAILED: re-applying the read-back config was rejected: $APPLY_SAME" >&2; exit 1; }
echo "ApplyConfig round-trip OK: $APPLY_SAME"

# Apply deliberately invalid config - must be rejected, with a real
# validation error (internal/haproxy.Manager.Validate's own fallback for
# an empty haproxy -c output - see its own comment), and must not
# disturb the running config.
python3 -c 'import json; print(json.dumps({"config": "this is not a valid haproxy config !!!"}))' > "$WORKDIR/apply-garbage.json"
APPLY_GARBAGE="$(curl -sk "${DASH_CERT[@]}" -X POST -H "Content-Type: application/json" -d @"$WORKDIR/apply-garbage.json" "${NODE_BASE}/api/haproxy/config")"
echo "$APPLY_GARBAGE" | grep -q '"accepted":false' || { echo "Dashboard test FAILED: garbage config wasn't rejected: $APPLY_GARBAGE" >&2; exit 1; }
echo "$APPLY_GARBAGE" | grep -q '"message":""' && { echo "Dashboard test FAILED: garbage config rejected with no error message: $APPLY_GARBAGE" >&2; exit 1; }
AFTER_GARBAGE_SHA256="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/config" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha256"])')"
[ "$AFTER_GARBAGE_SHA256" = "$ORIG_SHA256" ] || { echo "Dashboard test FAILED: running config changed after a rejected apply (was $ORIG_SHA256, now $AFTER_GARBAGE_SHA256)" >&2; exit 1; }
echo "ApplyConfig rejection OK: garbage config refused with a real error message, running config untouched"

# BackendList isn't implemented yet (falls through to
# UnimplementedHAProxyServiceServer - see internal/api/haproxy.go's own
# doc comment) - the relay must surface that as a real error, not a
# fake empty success.
BACKENDS_CODE="$(curl -sk "${DASH_CERT[@]}" -o /tmp/backends-resp.$$ -w '%{http_code}' "${NODE_BASE}/api/haproxy/backends")"
[ "$BACKENDS_CODE" = "502" ] && grep -q "Unimplemented" /tmp/backends-resp.$$ || { echo "Dashboard test FAILED: BackendList relay didn't surface Unimplemented (code=$BACKENDS_CODE): $(cat /tmp/backends-resp.$$)" >&2; rm -f /tmp/backends-resp.$$; exit 1; }
rm -f /tmp/backends-resp.$$
echo "BackendList relay OK: correctly surfaces the real not-yet-implemented error"

# The bootstrap config declares no file-backed maps - MapList must
# relay a genuinely empty list (protojson's omitempty drops an empty
# repeated field entirely, so "{}" is the correct empty response, not a
# gap - confirmed against a real boot before encoding this assertion).
MAPS_RESP="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/maps")"
[ "$MAPS_RESP" = "{}" ] || { echo "Dashboard test FAILED: expected an empty MapList response, got: $MAPS_RESP" >&2; exit 1; }
echo "MapList relay OK: genuinely empty (bootstrap config declares no file-backed maps)"

# MapUpdate against a map that doesn't exist must surface HAProxy's own
# real error through the relay, not a fake success - exercising a real
# success case needs a custom boot fixture with a file-backed map
# declared (already covered at the internal/haproxy layer by
# image-build.yml's own runtime map/ACL/certificate test).
MAPUPDATE_CODE="$(curl -sk "${DASH_CERT[@]}" -o /tmp/mapupdate-resp.$$ -w '%{http_code}' -X POST -H "Content-Type: application/json" -d '{"key":"k","value":"v","delete":false}' "${NODE_BASE}/api/haproxy/maps/nonexistent.map")"
[ "$MAPUPDATE_CODE" = "502" ] && grep -qi "map identifier\|no such\|unknown" /tmp/mapupdate-resp.$$ || { echo "Dashboard test FAILED: MapUpdate against a nonexistent map didn't surface a real error (code=$MAPUPDATE_CODE): $(cat /tmp/mapupdate-resp.$$)" >&2; rm -f /tmp/mapupdate-resp.$$; exit 1; }
rm -f /tmp/mapupdate-resp.$$
echo "MapUpdate relay OK: genuinely reaches the real node and surfaces its real error"

# CertificateList starts genuinely empty too (bootstrap config has no
# ssl bind/crt-list at all).
CERTS_RESP="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/certs")"
[ "$CERTS_RESP" = "{}" ] || { echo "Dashboard test FAILED: expected an empty CertificateList response, got: $CERTS_RESP" >&2; exit 1; }

# Real CertificateUpload/List/Delete round trip - a throwaway
# self-signed test cert, no crt_list (the cert store accepts an upload
# unconditionally; binding it into a crt-list needs a config that
# declares one, out of scope here - see internal/haproxy/runtime_certs.go's
# own doc comment). Built via a real FILE, not a $(...) substitution -
# command substitution strips the bundle's own trailing newline, which
# left HAProxy's stats-socket "set ssl cert <<\n<bundle>" heredoc
# waiting forever for the rest of a line that never arrives (a real gap
# in internal/haproxy.Manager.statsCommand's missing read deadline,
# fixed alongside this test - see its own comment - but the bundle
# still needs to be well-formed here to exercise the real success path).
openssl req -x509 -newkey ed25519 -keyout "$WORKDIR/testcert.key" -out "$WORKDIR/testcert.crt" -days 1 -nodes -subj "/CN=dashboard-test.example.com" >/dev/null 2>&1
cat "$WORKDIR/testcert.crt" "$WORKDIR/testcert.key" > "$WORKDIR/testcert-bundle.pem"
python3 -c '
import json, sys
bundle = open(sys.argv[1]).read()
json.dump({"name": "dashboard-test.pem", "pem_bundle": bundle}, open(sys.argv[2], "w"))
' "$WORKDIR/testcert-bundle.pem" "$WORKDIR/cert-upload.json"
UPLOAD_RESP="$(curl -sk "${DASH_CERT[@]}" -m 15 -X POST -H "Content-Type: application/json" -d @"$WORKDIR/cert-upload.json" "${NODE_BASE}/api/haproxy/certs")"
[ "$UPLOAD_RESP" = "{}" ] || { echo "Dashboard test FAILED: CertificateUpload didn't return success: $UPLOAD_RESP" >&2; exit 1; }
LIST_AFTER_UPLOAD="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/certs")"
echo "$LIST_AFTER_UPLOAD" | grep -qF '"name":"dashboard-test.pem"' || { echo "Dashboard test FAILED: uploaded cert not listed: $LIST_AFTER_UPLOAD" >&2; exit 1; }
DELETE_CODE="$(curl -sk "${DASH_CERT[@]}" -o /dev/null -w '%{http_code}' -X DELETE "${NODE_BASE}/api/haproxy/certs/dashboard-test.pem")"
[ "$DELETE_CODE" = "200" ] || { echo "Dashboard test FAILED: CertificateDelete returned $DELETE_CODE, want 200" >&2; exit 1; }
LIST_AFTER_DELETE_CERT="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/certs")"
[ "$LIST_AFTER_DELETE_CERT" = "{}" ] || { echo "Dashboard test FAILED: cert still listed after delete: $LIST_AFTER_DELETE_CERT" >&2; exit 1; }
echo "Certificate relay OK: real upload/list/delete round trip against the real node's cert store"

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
del_code="$(curl -s -b "$COOKIE_JAR" -X DELETE -o /dev/null -w '%{http_code}' "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes/${NODE_ID}")"
[ "$del_code" = "204" ] || { echo "Dashboard test FAILED: DELETE returned $del_code, want 204" >&2; exit 1; }
LIST_AFTER_DELETE="$(curl -s -b "$COOKIE_JAR" "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
if echo "$LIST_AFTER_DELETE" | grep -qF "\"id\":\"$NODE_ID\""; then
  echo "Dashboard test FAILED: node still listed after DELETE: $LIST_AFTER_DELETE" >&2
  exit 1
fi
after_delete_code="$(curl -sk -o /dev/null -w '%{http_code}' -m 3 --cert "$WORKDIR/admin.crt" --key "$WORKDIR/admin.key" "https://127.0.0.1:${NODE_LISTEN_PORT}/api/info" || true)"
[ "$after_delete_code" = "000" ] || { echo "Dashboard test FAILED: per-node port still answering after DELETE (got $after_delete_code)" >&2; exit 1; }
echo "Delete OK: node unregistered, its listener stopped accepting connections entirely"

# --- persistence across a restart ---
curl -s -b "$COOKIE_JAR" -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" -H "Content-Type: application/json" -d @"$WORKDIR/add-node.json" > /dev/null
kill "$DASHBOARD_PID"
wait "$DASHBOARD_PID" 2>/dev/null || true
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" -data-dir "$WORKDIR/data" > "$WORKDIR/dashboardd-restart.log" 2>&1 &
DASHBOARD_PID=$!
sleep 1

# Sessions are deliberately in-memory only (internal/auth's own doc
# comment) - they don't survive a restart, so the old cookie must now
# be rejected, and a fresh login is needed before the registry is
# reachable again. The admin password itself (auth.json, on disk) does
# survive, same as the node registry.
stale_session_code="$(curl -s -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
[ "$stale_session_code" = "401" ] || { echo "Dashboard test FAILED: a pre-restart session should not survive dashboardd restarting, got $stale_session_code" >&2; exit 1; }
relogin_code="$(curl -s -c "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/login" -H "Content-Type: application/json" -d '{"password":"dashboard-test-admin-pw"}')"
[ "$relogin_code" = "204" ] || { echo "Dashboard test FAILED: re-login after restart with the same (persisted) password should succeed, got $relogin_code" >&2; exit 1; }
echo "Auth persistence OK: the admin password survives a restart, the session doesn't - a fresh login is required"

PENDING_AFTER_RESTART="$(curl -s -b "$COOKIE_JAR" "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending")"
echo "$PENDING_AFTER_RESTART" | grep -qF "\"id\":\"$PENDING_ID\"" || { echo "Dashboard test FAILED: the pending self-registration didn't survive a dashboardd restart: $PENDING_AFTER_RESTART" >&2; exit 1; }
echo "Pending persistence OK: the self-registered node's pending entry survived a dashboardd restart"

RESTART_LIST="$(curl -s -b "$COOKIE_JAR" "http://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
echo "$RESTART_LIST" | grep -q '"name":"test-node"' || { echo "Dashboard test FAILED: node registry didn't survive a restart: $RESTART_LIST" >&2; cat "$WORKDIR/dashboardd-restart.log" >&2; exit 1; }
RESTART_PORT="$(echo "$RESTART_LIST" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["port"])')"
restart_relay_code="$(curl -sk -o /dev/null -w '%{http_code}' --cert "$WORKDIR/admin.crt" --key "$WORKDIR/admin.key" "https://127.0.0.1:${RESTART_PORT}/api/info" || true)"
[ "$restart_relay_code" = "200" ] || { echo "Dashboard test FAILED: per-node listener didn't come back after restart (got $restart_relay_code)" >&2; exit 1; }
echo "Restart persistence OK: node registry and per-node listener both survived a dashboardd restart against the same data directory"

echo "Dashboard test OK: add/list/relay/mTLS-gate/delete/restart-persistence all verified against a real running node"
