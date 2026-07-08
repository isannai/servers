#!/usr/bin/env bash
# build/linux/build.sh [version] [market|rendezvous|all]
#
# Build the servers for linux/amd64 (static, CGO off) into build/out/linux/.
# Used for dev/verify AND consumed by the docker-compose deploys (which mount
# build/out/linux/<bin>).
#
#   ./build/linux/build.sh                 # both, version "dev"
#   ./build/linux/build.sh 0.1.0           # both, stamped 0.1.0
#   ./build/linux/build.sh 0.1.0 market    # just market
set -euo pipefail
cd "$(dirname "$0")/../.."

VER="${1:-dev}"
WHAT="${2:-all}"
PKG="github.com/isannai/isann-servers/pkg/setup"
LDFLAGS="-X ${PKG}.MarketVersion=${VER} -X ${PKG}.RendezvousVersion=${VER}"
OUT="build/out/linux"
mkdir -p "$OUT"

build() {
  local cmd="$1"
  echo "  market/$cmd → $OUT/$cmd  (linux/amd64, static, v$VER)"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/$cmd" "./cmd/$cmd"
}

echo "=== isann-servers build (linux) v$VER ==="
case "$WHAT" in
  market)     build market ;;
  rendezvous) build rendezvous ;;
  all)        build market; build rendezvous ;;
  *) echo "usage: $0 [version] [market|rendezvous|all]" >&2; exit 1 ;;
esac
echo "done."
