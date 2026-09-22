#!/usr/bin/env bash
# Assembles the immutable squashfs rootfs from rootfs/base + the built
# pkgs/* outputs. Called by the top-level Makefile's future `image` target.
#
# TODO(Phase 1): not implemented yet - pkgs/* don't produce real build
# output until their Dockerfiles grow a "build" stage (see pkgs/*/Dockerfile
# TODOs). Fails loudly instead of silently producing an empty image.
set -euo pipefail

echo "rootfs/assemble.sh: not implemented yet (Phase 1) - see docs/architecture.md" >&2
exit 1
