#!/usr/bin/env bash
# Assembles the immutable rootfs: builds a squashfs image from rootfs/base
# + the given binaries/config, then computes its dm-verity hash tree.
# Requires mksquashfs (squashfs-tools) and veritysetup (cryptsetup-bin) on
# PATH - neither needs root privileges for this (only mounting/verifying
# a *live* dm-verity device does).
#
# Usage: rootfs/assemble.sh <out-dir> <init-bin> <haproxyosd-bin> <haproxy-bin> <haproxy-cfg>
#
# Writes to <out-dir>:
#   rootfs.squashfs   - the read-only root filesystem image
#   rootfs.verity     - its dm-verity hash tree
#   rootfs.roothash   - the root hash (hex), the one thing that must be
#                        trusted out-of-band (Phase 3+: embedded in the
#                        Secure-Boot-signed kernel cmdline/UKI, not shipped
#                        as a separate file on a real node)
set -euo pipefail

OUT_DIR="${1:?usage: $0 <out-dir> <init-bin> <haproxyosd-bin> <haproxy-bin> <haproxy-cfg>}"
INIT_BIN="${2:?usage: $0 <out-dir> <init-bin> <haproxyosd-bin> <haproxy-bin> <haproxy-cfg>}"
DAEMON_BIN="${3:?usage: $0 <out-dir> <init-bin> <haproxyosd-bin> <haproxy-bin> <haproxy-cfg>}"
HAPROXY_BIN="${4:?usage: $0 <out-dir> <init-bin> <haproxyosd-bin> <haproxy-bin> <haproxy-cfg>}"
HAPROXY_CFG="${5:?usage: $0 <out-dir> <init-bin> <haproxyosd-bin> <haproxy-bin> <haproxy-cfg>}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

mkdir -p "$WORKDIR"/{proc,sys,dev,run,var,tmp,sbin,usr/local/sbin,etc/haproxy}
install -m 0755 "$INIT_BIN" "$WORKDIR/sbin/init"
install -m 0755 "$DAEMON_BIN" "$WORKDIR/sbin/haproxyosd"
install -m 0755 "$HAPROXY_BIN" "$WORKDIR/usr/local/sbin/haproxy"
install -m 0644 "$HAPROXY_CFG" "$WORKDIR/etc/haproxy/haproxy.cfg"
# /run, /var, /tmp stay empty in the image itself; Phase 3's ephemeral
# overlay (not implemented yet) is what makes them writable on a booted
# node.

mkdir -p "$OUT_DIR"
# -all-root: every file/dir owned by uid=gid=0 regardless of who's
# running this script - there's no /etc/passwd on the target to resolve
# any other owner against, and a build run by a non-root developer must
# still produce a root-owned image.
# -root-mode 0755: mktemp -d's default 0700 on $WORKDIR would otherwise
# become the squashfs root directory's own mode (caught by mounting the
# very first build of this script and inspecting it - `ls` on the
# mounted root as non-root failed outright).
#
# The chroot jail (see cmd/haproxyosd's -haproxy-chroot-dir) is added as
# a pseudo file entry - mode 0000, not even readable by its own owner -
# rather than a real mkdir+chmod 000 in $WORKDIR: a genuinely
# unreadable/unenterable directory can't be read back by mksquashfs
# itself when it isn't running as root (caught the same way as the
# root-mode issue above: it silently vanished from the built image,
# `mksquashfs` only warned "Could not open ... skipping").
mksquashfs "$WORKDIR" "$OUT_DIR/rootfs.squashfs" -noappend -comp xz -all-root -root-mode 0755 \
  -p "var/empty D 0 0000 0 0"

veritysetup format "$OUT_DIR/rootfs.squashfs" "$OUT_DIR/rootfs.verity" > "$OUT_DIR/rootfs.verity.info"
grep "^Root hash:" "$OUT_DIR/rootfs.verity.info" | awk '{print $3}' > "$OUT_DIR/rootfs.roothash"

echo "Wrote $OUT_DIR/{rootfs.squashfs,rootfs.verity,rootfs.roothash}"
echo "Root hash: $(cat "$OUT_DIR/rootfs.roothash")"
