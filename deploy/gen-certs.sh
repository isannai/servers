#!/usr/bin/env bash
# Generate self-signed TLS certs into each deploy/<svc>/certs/ for LOCAL/TEST use.
# The configs reference certs/cert.pem + certs/key.pem when tls.enabled=true.
#
#   ./deploy/gen-certs.sh
#
# Production: use real certs (Let's Encrypt / your CA). NEVER commit private keys
# — certs/ is gitignored on purpose.
set -euo pipefail
cd "$(dirname "$0")"

for svc in market rendezvous; do
  mkdir -p "$svc/certs"
  openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
    -keyout "$svc/certs/key.pem" -out "$svc/certs/cert.pem" \
    -subj "/CN=localhost" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
  echo "  $svc/certs/{cert,key}.pem"
done
echo "done — self-signed (localhost). Set tls.enabled=true in the conf to use them."
