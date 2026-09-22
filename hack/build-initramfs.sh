#!/usr/bin/env bash
# Packages the statically-linked rootfs/init binary into a gzip-compressed
# cpio initramfs - the whole Phase 1 "rootfs" (no squashfs, no overlay,
# nothing else). Usage: hack/build-initramfs.sh <init-binary> <output.cpio.gz>
set -euo pipefail

INIT_BIN="${1:?usage: $0 <init-binary> <output.cpio.gz>}"
OUT="${2:?usage: $0 <init-binary> <output.cpio.gz>}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

mkdir -p "$WORKDIR"/{proc,sys,dev}
cp "$INIT_BIN" "$WORKDIR/init"
chmod 0755 "$WORKDIR/init"

(cd "$WORKDIR" && find . -print0 | cpio --null -o --format=newc 2>/dev/null) | gzip -9 > "$OUT"

echo "Wrote $OUT ($(du -h "$OUT" | cut -f1))"
