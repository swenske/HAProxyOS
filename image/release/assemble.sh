#!/usr/bin/env bash
# Assembles a "release bundle" - everything LifecycleService.Upgrade
# (internal/api/lifecycle.go) needs to install a new rootfs onto the
# currently-inactive A/B slot of an already-running node:
#
#   rootfs.squashfs         - same content rootfs/assemble.sh produces
#   rootfs.squashfs.sha256  - its sha256, hex, one line (what
#                              ImageSource.sha256 gets checked against)
#   rootfs.verity           - the matching dm-verity hash tree
#   uki-a.efi / uki-b.efi   - this rootfs's Unified Kernel Image, one
#                              pre-built copy per A/B slot
#
# The UKIs matter because the running node can never build one itself:
# `ukify`/`sbsign` don't exist on the target OS (no package manager, by
# design). image/disk/assemble.sh and image/disk/activate-slot.sh solve
# this for the *initial* image (both slots start with the same content,
# so both slots' UKIs are known at build time); Upgrade needs the exact
# same trick for whatever *new* content it's about to install - which
# is precisely why this bundle carries a pre-built UKI for both
# possible target slots, not just the squashfs/verity pair. Upgrade
# picks whichever one matches the slot it's actually switching to and
# uses it as-is: no PE manipulation, no signing, no build tooling, ever,
# on the node.
#
# Device paths baked into both UKIs' cmdlines assume the disk attaches
# as /dev/vda, same as every other script in this project - see
# image/disk/assemble.sh's own note on why that's a real, documented
# limitation, not an oversight.
#
# Usage: image/release/assemble.sh <out-dir> <bzImage> <rootfs-dir> [signing-key] [signing-cert]
# <rootfs-dir> must contain rootfs.squashfs/rootfs.verity/
# rootfs.roothash/rootfs.verity.info (see rootfs/assemble.sh).
# [signing-key]/[signing-cert] are optional, passed straight through to
# image/uki/assemble.sh - see its own comment.
set -euo pipefail

OUT_DIR="${1:?usage: $0 <out-dir> <bzImage> <rootfs-dir> [signing-key] [signing-cert]}"
KERNEL="${2:?usage: $0 <out-dir> <bzImage> <rootfs-dir> [signing-key] [signing-cert]}"
ROOTFS_DIR="${3:?usage: $0 <out-dir> <bzImage> <rootfs-dir> [signing-key] [signing-cert]}"
SIGNING_KEY="${4:-}"
SIGNING_CERT="${5:-}"

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$OUT_DIR"

cp "$ROOTFS_DIR/rootfs.squashfs" "$OUT_DIR/rootfs.squashfs"
cp "$ROOTFS_DIR/rootfs.verity" "$OUT_DIR/rootfs.verity"
cp "$ROOTFS_DIR/rootfs.roothash" "$OUT_DIR/rootfs.roothash"
sha256sum "$OUT_DIR/rootfs.squashfs" | awk '{print $1}' > "$OUT_DIR/rootfs.squashfs.sha256"

"$SELF_DIR/../uki/assemble.sh" "$OUT_DIR/uki-a.efi" "$KERNEL" "$ROOTFS_DIR" /dev/vda2 /dev/vda3 "$SIGNING_KEY" "$SIGNING_CERT"
"$SELF_DIR/../uki/assemble.sh" "$OUT_DIR/uki-b.efi" "$KERNEL" "$ROOTFS_DIR" /dev/vda4 /dev/vda5 "$SIGNING_KEY" "$SIGNING_CERT"

echo "Wrote release bundle to $OUT_DIR: rootfs.squashfs ($(cat "$OUT_DIR/rootfs.squashfs.sha256")), rootfs.verity, uki-a.efi, uki-b.efi"
