#!/usr/bin/env bash
# Generates a throwaway, self-signed RSA key + certificate for signing
# UKIs (image/uki/assemble.sh's optional signing-key/signing-cert args)
# and enrolling into a test VM's Secure Boot database
# (image/secureboot/enroll-vars.sh).
#
# This is explicitly a TEST/DEV key, generated fresh, never committed:
# it exists to prove the Secure Boot signing *mechanism* works
# end to end (hack/qemu-secureboot-test.sh), not to be a real
# project release key - that's image/secureboot/gen-production-key.sh
# (run once, offline, by a human - never by a build script on the fly)
# plus image-build.yml's own "production-signed release bundle" step,
# which reads the resulting private key from a GitHub Actions encrypted
# secret rather than ever committing it.
#
# Usage: image/secureboot/gen-test-key.sh <out-dir>
# Writes <out-dir>/{key.pem,cert.pem}.
set -euo pipefail

OUT_DIR="${1:?usage: $0 <out-dir>}"
mkdir -p "$OUT_DIR"

openssl req -x509 -newkey rsa:2048 \
  -keyout "$OUT_DIR/key.pem" -out "$OUT_DIR/cert.pem" \
  -nodes -days 3650 \
  -subj "/CN=Janus test signing key (not for production use)" \
  >/dev/null 2>&1

echo "Wrote $OUT_DIR/{key.pem,cert.pem} (throwaway test key - see this script's own comment)"
