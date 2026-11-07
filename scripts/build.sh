#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${VERSION:-2026.11.1}
BUILD_DATE=${BUILD_DATE:-2026-11-07}
GO=${GO:-go}
OUTPUT=${OUTPUT:-bin/janus}
# EDITION=fips builds the Janus FIPS edition: the Go Cryptographic Module
# v1.0.0 (CMVP certificate #5247) in FIPS 140-3 mode, and the fips build tag.
EDITION=${EDITION:-default}
case "$EDITION" in
  default) FIPSENV=""; TAGS="" ;;
  fips) FIPSENV="GOFIPS140=v1.0.0"; TAGS="-tags fips" ;;
  *) echo "EDITION must be default or fips" >&2; exit 1 ;;
esac
[[ "$VERSION" =~ ^[0-9]{4}\.([1-9]|1[0-2])\.[1-9][0-9]*$ ]] || { echo "Invalid YEAR.MONTH.RELEASE_NUMBER" >&2; exit 1; }
[[ "$BUILD_DATE" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || { echo "Invalid BUILD_DATE" >&2; exit 1; }
test -f web/dist/index.html || { echo "Build the web bundle with make web first" >&2; exit 1; }
if find web/dist -name '*.map' -print -quit | grep -q .; then
  echo "Refusing to embed source maps" >&2; exit 1
fi
# Fail rather than produce a release with an unstamped standalone binary.
for symbol in DefaultBuildVersion DefaultBuildDate DefaultBuildSHA; do
  grep -Rq "${symbol}" internal/config || { echo "Missing config build symbol: $symbol" >&2; exit 1; }
done
mkdir -p "$(dirname "$OUTPUT")"
GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.10} CGO_ENABLED=0 env $FIPSENV "$GO" build $TAGS -trimpath -buildvcs=false   -ldflags "-s -w -buildid= -X github.com/torvanis/janus/internal/config.DefaultBuildVersion=$VERSION -X github.com/torvanis/janus/internal/config.DefaultBuildDate=$BUILD_DATE -X github.com/torvanis/janus/internal/config.DefaultBuildSHA=unknown"   -o "$OUTPUT" ./cmd/janus
