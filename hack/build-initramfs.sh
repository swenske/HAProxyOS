#!/usr/bin/env bash
# Packages the statically-linked rootfs/init binary - plus any extra
# files (haproxyosd, haproxy, a bootstrap haproxy.cfg, ...) - into a
# gzip-compressed cpio initramfs. This is the whole "rootfs" for the
# QEMU boot-test milestones: no squashfs, no overlay, nothing else.
#
# Usage: hack/build-initramfs.sh <init-binary> <output.cpio.gz> [src:dest ...]
#   src  is a local file to include.
#   dest is its path inside the initramfs (e.g. sbin/haproxyosd).
set -euo pipefail

INIT_BIN="${1:?usage: $0 <init-binary> <output.cpio.gz> [src:dest ...]}"
OUT="${2:?usage: $0 <init-binary> <output.cpio.gz> [src:dest ...]}"
shift 2

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

mkdir -p "$WORKDIR"/{proc,sys,dev}
cp "$INIT_BIN" "$WORKDIR/init"
chmod 0755 "$WORKDIR/init"

for pair in "$@"; do
  src="${pair%%:*}"
  dest="${pair#*:}"
  mkdir -p "$WORKDIR/$(dirname "$dest")"
  cp -p "$src" "$WORKDIR/$dest" # -p: preserve the source's mode (executable bit for binaries, 0644 for config)
done

(cd "$WORKDIR" && find . -print0 | cpio --null -o --format=newc 2>/dev/null) | gzip -9 > "$OUT"

echo "Wrote $OUT ($(du -h "$OUT" | cut -f1))"
