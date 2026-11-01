#!/bin/bash
# Builds the toprelay Go engine as a universal (arm64 + x86_64) binary into the Mac app bundle's Contents/Helpers
# (not Contents/MacOS: the file system ignores case, so "toprelay" would replace the app's own "TopRelay"), and signs it with
# the app's identity and the hardened runtime (required for notarization). Run by the TopRelay target's
# "Bundle toprelay engine" build phase; also runnable by hand with TARGET_BUILD_DIR and CONTENTS_FOLDER_PATH set.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="${TARGET_BUILD_DIR:?}/${CONTENTS_FOLDER_PATH:?}/Helpers"
OUT="$OUT_DIR/toprelay"
export PATH="/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:$PATH"

GO="$(command -v go || true)"
if [ -z "$GO" ]; then
  echo "error: Go is not installed. Install it with: brew install go"
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$OUT_DIR"

cd "$REPO/agent"
for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" "$GO" build -trimpath -ldflags "-s -w" -o "$TMP/toprelay-$arch" ./cmd/toprelay
done
lipo -create -output "$OUT" "$TMP/toprelay-arm64" "$TMP/toprelay-amd64"

if [ "${CODE_SIGNING_ALLOWED:-YES}" = "YES" ] && [ -n "${EXPANDED_CODE_SIGN_IDENTITY:-}" ]; then
  TIMESTAMP="--timestamp=none"
  if [ "${CONFIGURATION:-Debug}" = "Release" ]; then TIMESTAMP="--timestamp"; fi
  codesign --force --options runtime $TIMESTAMP --sign "$EXPANDED_CODE_SIGN_IDENTITY" "$OUT"
fi
echo "toprelay engine: $(lipo -archs "$OUT") → $OUT"
