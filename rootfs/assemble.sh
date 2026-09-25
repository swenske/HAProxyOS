#!/usr/bin/env bash
# Assembles the immutable rootfs: builds a squashfs image from rootfs/base
# + the given binaries/config, then computes its dm-verity hash tree.
# Requires mksquashfs (squashfs-tools) and veritysetup (cryptsetup-bin) on
# PATH - neither needs root privileges for this (only mounting/verifying
# a *live* dm-verity device does).
#
# Usage: rootfs/assemble.sh <out-dir> <init-bin> <janusd-bin> <haproxy-bin> <haproxy-cfg> <selinux-policy>
#
# Writes to <out-dir>:
#   rootfs.squashfs   - the read-only root filesystem image
#   rootfs.verity     - its dm-verity hash tree
#   rootfs.roothash   - the root hash (hex), the one thing that must be
#                        trusted out-of-band (Phase 3+: embedded in the
#                        Secure-Boot-signed kernel cmdline/UKI, not shipped
#                        as a separate file on a real node)
set -euo pipefail

# Debian installs veritysetup (cryptsetup-bin) to /usr/sbin, which isn't
# guaranteed to be on PATH for a non-interactive/non-root shell even once
# the package is installed - confirmed on janus-runner01, where this
# script's own `mksquashfs` call (installed to /usr/bin, always on PATH)
# succeeded but `veritysetup` failed with "command not found" despite
# `apt-get install cryptsetup-bin` having just run cleanly in the same job.
export PATH="$PATH:/usr/sbin:/sbin"

USAGE="usage: $0 <out-dir> <init-bin> <janusd-bin> <haproxy-bin> <haproxy-cfg> <selinux-policy>"
OUT_DIR="${1:?$USAGE}"
INIT_BIN="${2:?$USAGE}"
DAEMON_BIN="${3:?$USAGE}"
HAPROXY_BIN="${4:?$USAGE}"
HAPROXY_CFG="${5:?$USAGE}"
SELINUX_POLICY="${6:?$USAGE}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

mkdir -p "$WORKDIR"/{proc,sys,dev,run,var,tmp,sbin,usr/local/sbin,etc/haproxy,etc/selinux}
install -m 0755 "$INIT_BIN" "$WORKDIR/sbin/init"
install -m 0755 "$DAEMON_BIN" "$WORKDIR/sbin/janusd"
install -m 0755 "$HAPROXY_BIN" "$WORKDIR/usr/local/sbin/haproxy"
install -m 0644 "$HAPROXY_CFG" "$WORKDIR/etc/haproxy/haproxy.cfg"
install -m 0644 "$SELINUX_POLICY" "$WORKDIR/etc/selinux/janus.policy"
# /run, /var, /tmp stay empty in the image itself; Phase 3's ephemeral
# overlay (not implemented yet) is what makes them writable on a booted
# node.

# Phase 4 cont'd (SELinux): the only three files on this rootfs whose
# type actually needs to be more specific than selinux/policy.conf's own
# fs_use_xattr default (squashfs_t) - the three real executables, so
# rootfs/init's own domain transitions (init_t -> janusd_t ->
# haproxy_t) have something to key off. Labeled via mksquashfs's own
# pseudo-file `x` action (below), not a plain `setfattr` on the source
# tree before mksquashfs runs, which turned out not to work at all here:
# a real setfattr call on $WORKDIR (mktemp -d's default, tmpfs-backed
# under WSL2) reports success and even reads back correctly with a
# direct getfattr - but mksquashfs itself, run either as the build user
# or as root, never picks the xattr up into the image regardless (traced
# with an isolated single-file reproduction, `-xattrs-include` didn't
# help either - mksquashfs's own directory-tree xattr scan just doesn't
# see it). The pseudo-file mechanism sidesteps that scan entirely, the
# same reason /var/empty's own mode-0000 pseudo-entry below already
# exists instead of a real chmod'd directory in $WORKDIR: mksquashfs
# sets the attribute directly while writing the image, rather than
# reading it back off a real inode first.
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
# The chroot jail (see cmd/janusd's -haproxy-chroot-dir) is added as
# a pseudo file entry - mode 0000, not even readable by its own owner -
# rather than a real mkdir+chmod 000 in $WORKDIR: a genuinely
# unreadable/unenterable directory can't be read back by mksquashfs
# itself when it isn't running as root (caught the same way as the
# root-mode issue above: it silently vanished from the built image,
# `mksquashfs` only warned "Could not open ... skipping").
mksquashfs "$WORKDIR" "$OUT_DIR/rootfs.squashfs" -noappend -comp xz -all-root -root-mode 0755 \
  -p "var/empty D 0 0000 0 0" \
  -p "sbin/init x security.selinux=system_u:object_r:init_exec_t" \
  -p "sbin/janusd x security.selinux=system_u:object_r:janusd_exec_t" \
  -p "usr/local/sbin/haproxy x security.selinux=system_u:object_r:haproxy_exec_t"

veritysetup format "$OUT_DIR/rootfs.squashfs" "$OUT_DIR/rootfs.verity" > "$OUT_DIR/rootfs.verity.info"
grep "^Root hash:" "$OUT_DIR/rootfs.verity.info" | awk '{print $3}' > "$OUT_DIR/rootfs.roothash"

echo "Wrote $OUT_DIR/{rootfs.squashfs,rootfs.verity,rootfs.roothash}"
echo "Root hash: $(cat "$OUT_DIR/rootfs.roothash")"
