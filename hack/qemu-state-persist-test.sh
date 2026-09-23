#!/usr/bin/env bash
# Proves rootfs/state-image.sh's persistent STATE partition actually
# survives a reboot - the gap explicitly left open when the ephemeral
# tmpfs overlay landed (see rootfs/init/main.go's mountEphemeral/
# mountState and docs/architecture.md's Phase 3 notes). Boots the same
# dm-verity-verified image against the SAME state.img file three times,
# that one attached as a third, *writable* virtio-blk drive (unlike the
# two read-only root drives), both frontend ports (8080 - the bootstrap
# default, 8081 - what a persisted "applied" config below switches to)
# forwarded on every boot so whichever one is actually live answers:
#
#   1. first boot - PKI hasn't been bootstrapped yet, haproxyosd must
#      log "pki: first boot - generated a new CA" and write it to
#      /etc/haproxyos/pki, which mountState mounts from the persistent
#      partition, not the ephemeral tmpfs. haproxyosd calls
#      syscall.Sync() right after writing those files (see
#      cmd/haproxyosd/main.go), so this isn't relying on QEMU's
#      shutdown-time cache flush to make them durable. Bootstrap
#      default config, so :8080 must answer.
#   2. second boot, same state.img (now holding boot 1's CA/certs) -
#      haproxyosd must NOT log that message again: internal/pki.
#      LoadOrBootstrap finds an existing ca.crt and loads it instead.
#      Still bootstrap default config (nothing's applied yet), so :8080
#      must still answer.
#   3. between boot 2 and boot 3, this script directly injects a new
#      haproxy/haproxy.cfg into state.img via `debugfs -w` (no mount, no
#      loop device, no root needed - deliberately: haproxyos-runner01 is
#      an unprivileged LXC container, where a real `mount -o loop`
#      failed outright with "failed to setup loop device" the first
#      time this test ran there, despite working fine locally) - a
#      config bound to :8081 instead, standing in for a real
#      HAProxyService.ApplyConfig RPC (already covered by
#      image-build.yml's own mTLS integration test step; what's under
#      test *here* is specifically whether internal/haproxy.Manager.
#      Apply's write target, /etc/haproxy/haproxy.cfg, actually lives on
#      persistent storage after rootfs/init's mountState bind-mounts it
#      there - not HAProxyService itself). Third boot must answer on
#      :8081, and must NOT answer on :8080 - proving haproxyosd started
#      HAProxy from the persisted config, not the squashfs's read-only
#      bootstrap default.
#
# Usage: hack/qemu-state-persist-test.sh <bzImage> <rootfs-dir> <state-image>
# <rootfs-dir> must contain rootfs.squashfs, rootfs.verity,
# rootfs.roothash and rootfs.verity.info (see rootfs/assemble.sh).
# <state-image> is a writable ext4 image (see rootfs/state-image.sh) -
# this script mutates it in place, including directly via `debugfs -w`
# (e2fsprogs, same package as rootfs/state-image.sh's own mkfs.ext4 -
# no extra tool needed, no root needed).
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <rootfs-dir> <state-image>}"
ROOTFS_DIR="${2:?usage: $0 <bzImage> <rootfs-dir> <state-image>}"
STATE_IMAGE="${3:?usage: $0 <bzImage> <rootfs-dir> <state-image>}"
HTTP_TIMEOUT_SECS="${QEMU_STATE_HTTP_TIMEOUT:-30}"
HOST_PORT_8080="${QEMU_STATE_TEST_PORT:-18083}"
HOST_PORT_8081="${QEMU_STATE_TEST_PORT2:-18084}"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"
INFO="$ROOTFS_DIR/rootfs.verity.info"
ROOTHASH="$(cat "$ROOTFS_DIR/rootfs.roothash")"
SALT="$(grep '^Salt:' "$INFO" | awk '{print $2}')"
DATA_BLOCKS="$(grep '^Data blocks:' "$INFO" | awk '{print $3}')"
DATA_BLOCK_SIZE="$(grep '^Data block size:' "$INFO" | awk '{print $4}')"
HASH_BLOCK_SIZE="$(grep '^Hash block size:' "$INFO" | awk '{print $4}')"
SECTORS=$(( DATA_BLOCKS * DATA_BLOCK_SIZE / 512 ))

dm_table() {
  echo "vroot,,,ro,0 $SECTORS verity 1 /dev/vda /dev/vdb $DATA_BLOCK_SIZE $HASH_BLOCK_SIZE $DATA_BLOCKS 1 sha256 $ROOTHASH $SALT"
}

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# Boots once against $STATE_IMAGE (mutated in place across calls) with
# both :8080 and :8081 forwarded, polls for HTTP 200 on whichever guest
# port $1 names, and writes the console log to $2. Sets $CODE_OTHER to
# the last-seen status code on the *other* port, for the boot-3 "must
# NOT still be on :8080" check.
CODE_OTHER=""
boot_and_wait_http() {
  local want_port="$1" log="$2"
  local other_port=8081 other_host_port="$HOST_PORT_8081"
  [ "$want_port" = "8081" ] && { other_port=8080; other_host_port="$HOST_PORT_8080"; }
  local want_host_port="$HOST_PORT_8080"
  [ "$want_port" = "8081" ] && want_host_port="$HOST_PORT_8081"

  qemu-system-x86_64 \
    -kernel "$KERNEL" \
    -append "console=ttyS0 panic=-1 dm-mod.create=\"$(dm_table)\" root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp" \
    -nographic -no-reboot -display none -m 256M \
    -drive file="$SQUASHFS",format=raw,if=virtio,readonly=on \
    -drive file="$VERITY",format=raw,if=virtio,readonly=on \
    -drive file="$STATE_IMAGE",format=raw,if=virtio \
    -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_PORT_8081}-:8081" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$log" \
    &
  QEMU_PID=$!

  local deadline=$((SECONDS + HTTP_TIMEOUT_SECS)) code=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${want_host_port}/" || true)"
    [ "$code" = "200" ] && break
    sleep 1
  done
  CODE_OTHER="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${other_host_port}/" || true)"

  kill "$QEMU_PID" 2>/dev/null || true
  wait "$QEMU_PID" 2>/dev/null || true
  QEMU_PID=""

  [ "$code" = "200" ]
}

BOOT1_LOG="$WORKDIR/boot1.log"
if ! boot_and_wait_http 8080 "$BOOT1_LOG"; then
  echo "State persist test FAILED: no HTTP 200 on :8080 on first boot within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$BOOT1_LOG" >&2
  exit 1
fi
if ! grep -q "$FIRST_BOOT_MSG" "$BOOT1_LOG"; then
  echo "State persist test FAILED: first boot didn't log '$FIRST_BOOT_MSG' - expected a fresh bootstrap against a blank state partition" >&2
  echo "--- console output ---" >&2
  cat "$BOOT1_LOG" >&2
  exit 1
fi
echo "First boot OK: bootstrap config live on :8080, bootstrapped a new CA onto the persistent STATE partition"

BOOT2_LOG="$WORKDIR/boot2.log"
if ! boot_and_wait_http 8080 "$BOOT2_LOG"; then
  echo "State persist test FAILED: no HTTP 200 on :8080 on second boot within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$BOOT2_LOG" >&2
  exit 1
fi
if grep -q "$FIRST_BOOT_MSG" "$BOOT2_LOG"; then
  echo "State persist test FAILED: second boot logged '$FIRST_BOOT_MSG' again - the CA didn't persist, /etc/haproxyos/pki is still effectively ephemeral" >&2
  echo "--- console output ---" >&2
  cat "$BOOT2_LOG" >&2
  exit 1
fi
echo "Second boot OK: loaded the existing CA from the persistent STATE partition - no re-bootstrap"

# Stand in for a real ApplyConfig RPC: write straight to the persistent
# haproxy/ subdirectory the way internal/haproxy.Manager.Apply's own
# os.WriteFile(m.ConfigPath, ...) would, once /etc/haproxy is
# bind-mounted from it (see rootfs/init/main.go's mountState).
APPLIED_CFG="$WORKDIR/applied-haproxy.cfg"
cat > "$APPLIED_CFG" <<'EOF'
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
    http-request return status 200 content-type text/plain string "HAProxyOS: applied config is live\n"
EOF
# `rm` first: debugfs's `write` refuses to overwrite an existing file.
# The rm's own "file not found"-style output (there's always something
# there already, from mountState's first-boot seeding) is expected and
# discarded rather than treated as this command's failure.
debugfs -w -R "rm haproxy/haproxy.cfg" "$STATE_IMAGE" >/dev/null 2>&1 || true
debugfs -w -R "write $APPLIED_CFG haproxy/haproxy.cfg" "$STATE_IMAGE"
echo "Applied a config bound to :8081 directly onto the persistent STATE partition (standing in for ApplyConfig)"

BOOT3_LOG="$WORKDIR/boot3.log"
if ! boot_and_wait_http 8081 "$BOOT3_LOG"; then
  echo "State persist test FAILED: no HTTP 200 on :8081 on third boot within ${HTTP_TIMEOUT_SECS}s - the persisted config wasn't picked up" >&2
  echo "--- console output ---" >&2
  cat "$BOOT3_LOG" >&2
  exit 1
fi
if [ "$CODE_OTHER" = "200" ]; then
  echo "State persist test FAILED: :8080 still answered HTTP 200 on the third boot - haproxy is still running the bootstrap default, not the persisted config" >&2
  echo "--- console output ---" >&2
  cat "$BOOT3_LOG" >&2
  exit 1
fi
echo "Third boot OK: HAProxy started from the persisted, previously-applied config (:8081 live, :8080 not) - applied config survives a reboot"
