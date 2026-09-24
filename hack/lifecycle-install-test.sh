#!/usr/bin/env bash
# Proves LifecycleService.Install actually partitions a genuinely blank
# disk from scratch (internal/diskimage + github.com/diskfs/go-diskfs -
# sgdisk/mtools/mkfs.ext4 don't exist on the target OS, see internal/
# api/install.go's own doc comment) and produces a real, independently
# bootable HAProxyOS image, and that its two safety refusals - an
# already-installed disk, and the disk this node itself is currently
# booted from - both actually reject in practice, not just in code
# review.
#
# Unlike every other lifecycle test in this project, the Install call
# itself doesn't need to run inside a VM: Install has no A/B/STATE
# machinery of its own to depend on (it's what *creates* that machinery
# on a blank target), so haproxyosd runs natively on the test host here
# - the same pattern image-build.yml's own "HAProxy gRPC API integration
# test" step already uses, reading its bootstrapped PKI creds straight
# off -pki-dir rather than scraping a QEMU console log for them. Only
# the *result* - is the disk Install just wrote genuinely bootable? - is
# verified under a real VM, the same standard every other "is this
# actually bootable" claim in this project is held to.
#
#   1. build a release bundle (image/release/assemble.sh) from the
#      *existing* rootfs build output - genuinely-new-content isn't the
#      point here, hack/qemu-lifecycle-upgrade-test.sh already proves
#      that.
#   2. run haproxyosd natively (root, for CAP_SYS_CHROOT - haproxy's
#      chroot() needs it, same reasoning as image-build.yml's own
#      integration test step), read its bootstrapped admin/CA creds
#      straight from -pki-dir.
#   3. call `haproxyosctl lifecycle install` against a freshly
#      truncated, empty *file* standing in for a blank disk -
#      go-diskfs works identically against a real block device or a
#      plain file (confirmed empirically - a full GPT+ext4+FAT32 disk
#      built this way, with real bundle content, booted successfully
#      under OVMF - before this script was ever written), so a file is
#      exactly as valid a target as a real /dev/sdX for what's being
#      tested here.
#   4. call Install a *second* time against the same, now-installed
#      file - must be refused (refuseIfAlreadyInstalled).
#   5. boot the installed disk.img under real OVMF, single virtio-blk
#      drive, no -kernel/-append (same pattern hack/
#      qemu-uefi-ab-boot-test.sh uses) - must answer real HTTP 200 and
#      log a fresh PKI bootstrap, proving Install's output isn't just
#      "the right bytes in the right place" as far as a static
#      inspection could tell.
#   6. extract fresh PKI creds from the *booted* instance's own STATE
#      partition (via debugfs, same reasoning as every other lifecycle
#      test here - a script can't watch a live console the way a human
#      doing this for real would), then call `haproxyosctl lifecycle
#      install` against /dev/vda -
#      the disk this now-running instance actually booted from - and
#      confirm it's refused (refuseIfCurrentBootDisk). This is the one
#      guard that structurally can't be tested in step 2-4's native
#      process, which has no real dm-mod.create= cmdline to compare
#      against at all.
#   7. bonus: call Rollback on the same booted instance and confirm it
#      comes back up healthy on slot B - since Install wrote *identical*
#      content and UKI to both slots, this proves that claim for real
#      rather than only ever having booted slot A.
#
# Usage: hack/lifecycle-install-test.sh <rootfs-dir> <bzImage> <haproxy-bin> <haproxyosd-bin> <haproxyosctl-bin>
# <rootfs-dir> must contain rootfs.squashfs/rootfs.verity/rootfs.roothash
# (rootfs/assemble.sh's output for the *existing* build).
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

ROOTFS_DIR="${1:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <haproxyosd-bin> <haproxyosctl-bin>}"
KERNEL="${2:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <haproxyosd-bin> <haproxyosctl-bin>}"
HAPROXY_BIN="${3:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <haproxyosd-bin> <haproxyosctl-bin>}"
HAPROXYOSD_BIN="${4:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <haproxyosd-bin> <haproxyosctl-bin>}"
CTL_REL="${5:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <haproxyosd-bin> <haproxyosctl-bin>}"
CTL="$(cd "$(dirname "$CTL_REL")" && pwd)/$(basename "$CTL_REL")"

# 512MiB comfortably clears internal/diskimage.Compute's real minimum
# (ESP 64 + 2x(DATA 64 + HASH 4) + MinStateMB 16 = 216MiB) while staying
# gentle on $WORKDIR, which - via mktemp -d - typically lands on a
# tmpfs (RAM-backed, often much smaller than real disk space).
DISK_MB="${INSTALL_TEST_DISK_MB:-512}"
HTTP_TIMEOUT_SECS="${INSTALL_TEST_HTTP_TIMEOUT:-40}"
HOST_PORT="${INSTALL_TEST_PORT:-18097}"
HOST_GRPC_PORT="${INSTALL_TEST_GRPC_PORT:-18098}"
NATIVE_PORT="${INSTALL_TEST_NATIVE_GRPC_PORT:-19507}"
MARKER="HAPROXYOS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
HAPROXYOSD_PID=""
QEMU_PID=""
# Real bug caught by a real CI run, not assumed: this cleanup trap used
# to kill the native haproxy child with `sudo pkill -f "$HAPROXY_BIN"`
# (e.g. "build/haproxy") - but `pkill -f` matches the *entire* command
# line of every process, and this script's own argv (`./hack/
# lifecycle-install-test.sh ... build/haproxy ...`) contains that exact
# string as one of its own positional arguments. The trap ended up
# killing *this script's own process*, mid-trap, right after its very
# last echo - `make` reported it as "Terminated" (exit 2) despite every
# single check already having passed and printed. Fixed by killing the
# real haproxy process by its own pid (haproxyosd's default
# `-haproxy-pid` path), never by a fuzzy command-line pattern match.
# cmd/haproxyosd has no SIGTERM handler that propagates to its haproxy
# child, so killing haproxyosd alone (below) leaves haproxy an orphan,
# reparented to init, still bound to :8080 - confirmed the hard way on
# haproxyos-runner01 itself: a real CI run's *next* job started with
# :8080 already taken, tracked back to a leftover `build/haproxy`
# process whose start time lined up exactly with this test's own native
# phase. A first fix tried killing by pid from haproxy's own -p pid
# file (/run/haproxyos/haproxy.pid) - wrong in a different way:
# internal/haproxy.Manager's seamless-reload path (-sf <old pid>, taken
# whenever that pid file already exists from an earlier run of this
# same test) leaves the *previous* instance to soft-stop-drain rather
# than exit outright, and repeated local runs showed the pid file can
# end up missing entirely while multiple old haproxy processes are
# still very much alive - confirmed by hand: `sudo cat` on the "current"
# pid file came back "No such file or directory" while `pgrep -x
# haproxy` still listed several. `pkill -x haproxy` (exact process-name
# match, not `-f`'s fuzzy full-command-line match - see this file's
# earlier comment on why `-f` is the wrong tool here) sidesteps needing
# the pid file at all, confirmed to actually kill a real lingering
# instance with a plain SIGTERM in well under a second.
kill_native_haproxy() {
  sudo pkill -x haproxy 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    pgrep -x haproxy >/dev/null 2>&1 || return 0
    sleep 0.5
  done
  sudo pkill -x -9 haproxy 2>/dev/null || true
}
cleanup() {
  [ -n "$HAPROXYOSD_PID" ] && sudo kill "$HAPROXYOSD_PID" 2>/dev/null || true
  kill_native_haproxy
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  # sudo, not plain rm: $WORKDIR/pki is 0700 root-owned
  # (internal/pki.LoadOrBootstrap, run under sudo above).
  sudo rm -rf "$WORKDIR"
}
trap cleanup EXIT

# =========================================================================
# Part 1: build the release bundle Install will consume.
# =========================================================================
BUNDLE="$WORKDIR/bundle"
"$SELF_DIR/../image/release/assemble.sh" "$BUNDLE" "$KERNEL" "$ROOTFS_DIR"
SHA256="$(cat "$BUNDLE/rootfs.squashfs.sha256")"

# =========================================================================
# Part 2: run haproxyosd natively and install onto a blank file.
# =========================================================================
PKI_DIR="$WORKDIR/pki"
sudo mkdir -p /run/haproxyos /etc/haproxy /etc/haproxyos
sudo cp "$SELF_DIR/../rootfs/base/etc/haproxy/haproxy.cfg" /etc/haproxy/haproxy.cfg
sudo mkdir -p /var/empty && sudo chmod 000 /var/empty

sudo "$HAPROXYOSD_BIN" -addr ":$NATIVE_PORT" -haproxy-binary "$HAPROXY_BIN" -pki-dir "$PKI_DIR" \
  > "$WORKDIR/native.log" 2>&1 &
HAPROXYOSD_PID=$!

# internal/pki.LoadOrBootstrap creates -pki-dir 0700, owned by root
# (haproxyosd runs under sudo, for CAP_SYS_CHROOT) - the invoking
# non-root user can't even traverse into it, so every check/read
# against $PKI_DIR needs sudo too, not just the haproxyosctl calls.
DEADLINE=$((SECONDS + 20))
while ! sudo test -s "$PKI_DIR/admin.crt" && [ "$SECONDS" -lt "$DEADLINE" ]; do sleep 1; done
if ! sudo test -s "$PKI_DIR/admin.crt"; then
  echo "Install test FAILED: haproxyosd never bootstrapped PKI at $PKI_DIR within 20s" >&2
  echo "--- native haproxyosd log ---" >&2; cat "$WORKDIR/native.log" >&2
  exit 1
fi
NATIVE_CTL_ARGS=(-endpoint "127.0.0.1:${NATIVE_PORT}" -ca "$PKI_DIR/ca.crt" -cert "$PKI_DIR/admin.crt" -key "$PKI_DIR/admin.key")
echo "Native haproxyosd OK: PKI bootstrapped at $PKI_DIR"

BLANK_DISK="$WORKDIR/blank-disk.img"
truncate -s "${DISK_MB}M" "$BLANK_DISK"

INSTALL_OUT="$(sudo "$CTL" "${NATIVE_CTL_ARGS[@]}" lifecycle install -sha256 "$SHA256" "$BLANK_DISK" "$BUNDLE")"
echo "$INSTALL_OUT"
if ! echo "$INSTALL_OUT" | grep -qi '\[done '; then
  echo "Install test FAILED: Install never reached the 'done' stage" >&2
  exit 1
fi
sudo chown "$(id -u):$(id -g)" "$BLANK_DISK"
echo "Part 1 OK: Install partitioned a blank file from scratch and wrote a full image"

# --- verify the partition table matches image/disk/assemble.sh's own
# convention exactly, independent of go-diskfs's own view of what it
# wrote ---
sgdisk -p "$BLANK_DISK" | grep -q "ESP" || { echo "Install test FAILED: no ESP partition found by sgdisk" >&2; exit 1; }
for name in BOOT-A-DATA BOOT-A-HASH BOOT-B-DATA BOOT-B-HASH STATE; do
  sgdisk -p "$BLANK_DISK" | grep -q "$name" || { echo "Install test FAILED: no $name partition found by sgdisk" >&2; sgdisk -p "$BLANK_DISK" >&2; exit 1; }
done
echo "Part 2 OK: partition table verified independently via sgdisk"

# --- Install onto an already-installed disk must be refused ---
if sudo "$CTL" "${NATIVE_CTL_ARGS[@]}" lifecycle install -sha256 "$SHA256" "$BLANK_DISK" "$BUNDLE" 2>"$WORKDIR/reinstall-denied.log"; then
  echo "Install test FAILED: a second Install onto the already-installed disk succeeded, want a refusal" >&2
  exit 1
fi
grep -qi "already has a" "$WORKDIR/reinstall-denied.log" || { echo "Install test FAILED: expected an 'already has a ... partition' refusal, got:" >&2; cat "$WORKDIR/reinstall-denied.log" >&2; exit 1; }
echo "Part 3 OK: a second Install onto the same disk was correctly refused"

sudo kill "$HAPROXYOSD_PID" 2>/dev/null || true
wait "$HAPROXYOSD_PID" 2>/dev/null || true
HAPROXYOSD_PID=""
kill_native_haproxy

# =========================================================================
# Part 4: boot the installed disk for real and confirm it's genuinely
# bootable - then exercise the one refusal that needs a real boot to
# test at all (the disk this node is currently running from).
# =========================================================================
LOG="$WORKDIR/console.log"
OVMF_VARS="$WORKDIR/vars.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$BLANK_DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

DEADLINE=$((SECONDS + HTTP_TIMEOUT_SECS))
CODE=""
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  CODE="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT}/" || true)"
  [ "$CODE" = "200" ] && break
  sleep 1
done
if [ "$CODE" != "200" ]; then
  echo "Install test FAILED: the installed disk never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
if ! grep -q "$MARKER" "$LOG" || ! grep -q "$FIRST_BOOT_MSG" "$LOG"; then
  echo "Install test FAILED: installed disk's first boot didn't show both the boot marker and a fresh CA bootstrap" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "Part 4 OK: the installed disk is genuinely, independently bootable under real UEFI - HTTP 200, fresh PKI bootstrap"

# --- extract fresh PKI creds from the booted instance's own STATE
# partition (partition 6, image/disk/assemble.sh's fixed convention -
# Install itself follows it too) ---
STATE_START_SECTOR="$(sgdisk -i 6 "$BLANK_DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$BLANK_DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$BLANK_DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Install test FAILED: couldn't extract pki/$f from the booted disk's STATE partition" >&2; exit 1; }
done
BOOTED_CTL_ARGS=(-endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key")

# --- Install targeting the disk this instance is currently booted
# from (/dev/vda - the only drive attached) must be refused ---
if "$CTL" "${BOOTED_CTL_ARGS[@]}" lifecycle install -sha256 "$SHA256" /dev/vda /etc/.state 2>"$WORKDIR/self-install-denied.log"; then
  echo "Install test FAILED: Install against this node's own boot disk (/dev/vda) succeeded, want a refusal" >&2
  exit 1
fi
grep -qi "currently booted from" "$WORKDIR/self-install-denied.log" || { echo "Install test FAILED: expected a 'currently booted from' refusal, got:" >&2; cat "$WORKDIR/self-install-denied.log" >&2; exit 1; }
echo "Part 5 OK: Install against this node's own currently-booted disk was correctly refused"

# --- bonus: Rollback must work too, proving slot B's content (written
# identically to slot A by Install) is genuinely valid, not just slot
# A's ---
ROLLBACK_OUT="$("$CTL" "${BOOTED_CTL_ARGS[@]}" lifecycle rollback)"
echo "$ROLLBACK_OUT"
echo "$ROLLBACK_OUT" | grep -qi "slot B" || { echo "Install test FAILED: Rollback didn't report switching to slot B" >&2; exit 1; }

DEADLINE=$((SECONDS + HTTP_TIMEOUT_SECS))
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  CODE="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT}/" || true)"
  [ "$CODE" = "200" ] && [ "$(grep -c "$MARKER" "$LOG" 2>/dev/null || echo 0)" -ge 2 ] && break
  sleep 1
done
if [ "$CODE" != "200" ] || [ "$(grep -c "$MARKER" "$LOG")" -lt 2 ]; then
  echo "Install test FAILED: slot B (after Rollback) never came back up healthy" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
if grep -q "verity 1 /dev/vda2 /dev/vda3" "$LOG" && ! grep -q "verity 1 /dev/vda4 /dev/vda5" "$LOG"; then
  echo "Install test FAILED: console never shows a boot from slot B's partitions (/dev/vda4+5)" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "Part 6 OK: Rollback onto slot B (Install's identical second copy) works too"

# Explicit kill+wait here, not just the exit trap's best-effort one -
# leaving a real reboot loop's QEMU process to the trap alone risks the
# harness reaping it out from under the script before it's actually
# exited, which can surface as a spurious non-zero exit for this
# otherwise-fully-passed test.
kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

echo "Install test OK: a real gRPC LifecycleService.Install call partitioned a blank disk from scratch, refused an already-installed one and this node's own boot disk, and produced a genuinely bootable, both-slots-valid image"
