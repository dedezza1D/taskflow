#!/usr/bin/env bash
# Generate a self-signed certificate for LOCAL DEVELOPMENT ONLY.
#
# The point is not to be trusted — browsers will warn, and that is correct for a
# certificate nobody vouched for. The point is that the dev stack speaks the same
# protocol as production, so a Secure cookie, an HSTS header or a mixed-content
# bug shows up here instead of after deploy.
#
# Never use the output in production: the key is written unencrypted and lives in
# the repo's ignored certs/ directory. Production certificates come from a CA.
set -euo pipefail

CERT_DIR="${CERT_DIR:-deploy/nginx/certs}"
DAYS="${DAYS:-365}"

# Git Bash on Windows rewrites arguments that look like absolute paths, which
# mangles openssl's "/CN=..." subject into a drive path. Harmless elsewhere.
export MSYS_NO_PATHCONV=1

mkdir -p "$CERT_DIR"

if [[ -f "$CERT_DIR/cert.pem" && "${FORCE:-0}" != "1" ]]; then
  echo "certificate already exists at $CERT_DIR/cert.pem (FORCE=1 to regenerate)"
  exit 0
fi

# SANs, not just CN: every current browser ignores commonName entirely and will
# reject a certificate whose subjectAltName does not match the host.
openssl req -x509 -nodes -newkey rsa:2048 \
  -keyout "$CERT_DIR/key.pem" \
  -out "$CERT_DIR/cert.pem" \
  -days "$DAYS" \
  -subj "/CN=localhost/O=TaskFlow Development/OU=NOT FOR PRODUCTION" \
  -addext "subjectAltName=DNS:localhost,DNS:taskflow.local,IP:127.0.0.1,IP:::1" \
  -addext "basicConstraints=critical,CA:FALSE" \
  -addext "keyUsage=critical,digitalSignature,keyEncipherment" \
  -addext "extendedKeyUsage=serverAuth" \
  2>/dev/null

chmod 600 "$CERT_DIR/key.pem"

echo "self-signed certificate written to $CERT_DIR (valid $DAYS days)"
echo "browsers will warn — expected, and the reason this is dev-only"
