#!/bin/sh
# Build Linux releases and sign the exact manifest bytes. Requires Go and sha256sum.
set -eu
if [ "$#" -ne 3 ]; then
  printf 'Usage: %s vX.Y.Z PRIVATE_KEY HTTPS_RELEASE_BASE_URL\n' "$0" >&2
  exit 2
fi
VERSION=$1
PRIVATE_KEY=$2
BASE_URL=${3%/}
case "$BASE_URL" in https://*) ;; *) echo 'Release base URL must use HTTPS' >&2; exit 2 ;; esac
case "$BASE_URL" in *\"*|*\\*|*' '*|*'?'*|*'#'*|*"$(printf '\t')"*) echo 'Release URL must not contain quotes, whitespace, query or fragment' >&2; exit 2 ;; esac
if ! printf '%s\n' "$VERSION" | LC_ALL=C grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  echo 'Version must be vX.Y.Z' >&2; exit 2
fi
[ -f "$PRIVATE_KEY" ] || { echo 'Private key file not found' >&2; exit 2; }
PRIVATE_KEY=$(CDPATH= cd -- "$(dirname -- "$PRIVATE_KEY")" && pwd)/$(basename -- "$PRIVATE_KEY")
GO_BIN=${GO_BIN:-go}
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT="$ROOT/dist/$VERSION"
[ ! -e "$OUT" ] || { echo 'Release output already exists; use a new version or review/remove it manually' >&2; exit 2; }
mkdir -p "$OUT"
TEMP=$(mktemp -d)
trap 'rm -rf "$TEMP"' EXIT HUP INT TERM
cd "$ROOT"
"$GO_BIN" build -trimpath -o "$TEMP/signer" .
printf '{"version":"%s","assets":[' "$VERSION" > "$OUT/manifest.json"
SEP=
for ARCH in amd64 arm64; do
  NAME="openwebui-proxy_${VERSION}_linux_${ARCH}"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" "$GO_BIN" build -trimpath -ldflags="-s -w -X main.Version=$VERSION" -o "$OUT/$NAME" .
  HASH=$(sha256sum "$OUT/$NAME" | cut -d ' ' -f 1)
  SIZE=$(wc -c < "$OUT/$NAME" | tr -d '[:space:]')
  printf '%s{"os":"linux","arch":"%s","url":"%s/%s","sha256":"%s","size":%s}' "$SEP" "$ARCH" "$BASE_URL" "$NAME" "$HASH" "$SIZE" >> "$OUT/manifest.json"
  SEP=,
done
printf ']}\n' >> "$OUT/manifest.json"
"$TEMP/signer" ota sign "$OUT/manifest.json" "$PRIVATE_KEY" "$OUT/manifest.json.sig"
printf 'Signed release written to %s\nUpload its contents to %s without modifying manifest.json.\n' "$OUT" "$BASE_URL"
