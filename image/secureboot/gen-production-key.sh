#!/usr/bin/env bash
# Generates HAProxyOS's real Secure Boot signing key + certificate -
# unlike gen-test-key.sh (throwaway, regenerated fresh every CI run),
# this is meant to be run ONCE, by a human, outside of CI: the result
# is a long-term root of trust every HAProxyOS node's firmware will be
# asked to enroll into its Secure Boot db. Losing the private key means
# losing the ability to sign future updates for already-enrolled nodes;
# leaking it means anyone can sign a UKI those nodes will boot.
#
# No HSM in this project's threat model (single maintainer, self-hosted
# CI) - the private key is meant to be uploaded as a GitHub Actions
# encrypted secret (see .github/workflows/image-build.yml's own
# "production-signed release bundle" step, gated on that secret
# existing) and then backed up somewhere safe *outside* this working
# directory (a password manager, an encrypted volume, ...) - this
# script deliberately does not do that upload itself, or delete
# anything, since both are exactly the kind of irreversible step that
# needs a human actually looking at what's happening.
#
# The certificate half is NOT secret - it has to be shipped and
# enrolled into every node's firmware db, so it's meant to be committed
# to the repo (image/secureboot/production-cert.pem) once generated.
# Only the private key needs to stay out of git entirely.
#
# 20 years validity, not gen-test-key.sh's 10: rotating this cert means
# re-enrolling it into every already-deployed node's firmware by hand
# (there's no remote db-rotation flow in this project yet), so it's
# deliberately long-lived rather than something to renew casually.
#
# Usage: image/secureboot/gen-production-key.sh <out-dir>
# Writes <out-dir>/{key.pem,cert.pem}. Refuses to overwrite an existing
# key.pem - this is meant to run exactly once ever, not casually rerun.
set -euo pipefail

OUT_DIR="${1:?usage: $0 <out-dir>}"
mkdir -p "$OUT_DIR"

if [ -e "$OUT_DIR/key.pem" ]; then
  echo "$OUT_DIR/key.pem already exists - refusing to overwrite a real signing key. Move it aside first if you genuinely mean to generate a new one (and understand that means re-enrolling every already-deployed node's firmware)." >&2
  exit 1
fi

umask 077
openssl req -x509 -newkey rsa:4096 \
  -keyout "$OUT_DIR/key.pem" -out "$OUT_DIR/cert.pem" \
  -nodes -days 7300 \
  -subj "/CN=HAProxyOS Secure Boot signing key" \
  >/dev/null 2>&1

echo "Wrote $OUT_DIR/{key.pem,cert.pem} - HAProxyOS's real Secure Boot signing key."
echo
echo "Next steps (do these now, in this order):"
echo "  1. Back up $OUT_DIR/key.pem somewhere safe and durable (a password"
echo "     manager, an encrypted volume, offline storage) - it is the only"
echo "     copy outside of wherever you upload it next."
echo "  2. Upload it as a GitHub Actions secret named SECUREBOOT_SIGNING_KEY"
echo "     on this repo, e.g.: gh secret set SECUREBOOT_SIGNING_KEY < $OUT_DIR/key.pem"
echo "  3. Commit $OUT_DIR/cert.pem into the repo as"
echo "     image/secureboot/production-cert.pem (the certificate is public,"
echo "     safe to commit - only key.pem is sensitive)."
echo "  4. Once 1-2 are done, consider deleting the local $OUT_DIR/key.pem -"
echo "     this script does not do that for you."
