#!/usr/bin/env bash
# Builds an OVMF NVRAM ("vars") file with the given certificate enrolled
# as Platform Key (PK), Key Exchange Key (KEK), *and* into the
# signature database (db) - all three, with Secure Boot turned on.
#
# db is the one that actually matters for whether a given signed image
# is allowed to boot; PK/KEK only govern who's allowed to update the
# Secure Boot variables themselves. `virt-fw-vars --enroll-cert`
# (systemd-independent, python3-virt-firmware) looked like the
# convenience shortcut for all of this, but empirically only populates
# PK and KEK, never db - confirmed by booting a *correctly* signed UKI
# against vars built with --enroll-cert and getting "Access Denied"
# anyway, then printing the vars store (`virt-fw-vars -p`) and finding
# no db variable in it at all. Using the explicit --set-pk/--add-kek/
# --add-db flags instead (all three, same cert) is what actually works
# - confirmed by that same UKI booting successfully once db had the
# cert in it.
#
# Requires python3-virt-firmware (virt-fw-vars) - doesn't need root.
#
# Usage: image/secureboot/enroll-vars.sh <out-vars.fd> <cert.pem> [ovmf-vars-template]
# [ovmf-vars-template] defaults to /usr/share/OVMF/OVMF_VARS_4M.fd.
set -euo pipefail

OUT="${1:?usage: $0 <out-vars.fd> <cert.pem> [ovmf-vars-template]}"
CERT="${2:?usage: $0 <out-vars.fd> <cert.pem> [ovmf-vars-template]}"
TEMPLATE="${3:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

[ -f "$TEMPLATE" ] || { echo "OVMF vars template not found at $TEMPLATE (package: ovmf)" >&2; exit 1; }

# Any well-formed GUID works as the "owner" of our custom cert entries -
# nothing here reads it back or compares it against anything else.
GUID="$(python3 -c 'import uuid; print(uuid.uuid4())')"

virt-fw-vars -i "$TEMPLATE" \
  --set-pk "$GUID" "$CERT" \
  --add-kek "$GUID" "$CERT" \
  --add-db "$GUID" "$CERT" \
  --sb \
  -o "$OUT" \
  >/dev/null

echo "Wrote $OUT ($CERT enrolled as PK+KEK+db, Secure Boot enabled)"
