#!/usr/bin/env bash
# Build static Linux/Windows binaries, the release packages and the update folder.
#   ./build.sh  -> dist/dumpcat-linux-{amd64,arm64}, dist/dumpcat-<ver>-linux.tar.gz,
#                  dist/dumpcat-windows-amd64.exe, dist/dumpcat-<ver>-windows-amd64.zip,
#                  dist/update/  (upload its content to your release server)
#
#   UPDATE_URL=https://downloads.dumpcat.cc ./build.sh             # built-in release server
#   VERSION=1.1.0 ./build.sh                                       # override the VERSION file
#   UPDATE_SIGNING_KEY=/secure/dumpcat-release.key ./build.sh      # sign latest.json, trust that key
#   UPDATE_PUBLIC_KEYS=KEY[,KEY] ./build.sh                        # also trust these public keys (key rotation)
#   SKIP_TESTS=1 ./build.sh
set -euo pipefail
cd "$(dirname "$0")"
VERSION="${VERSION:-$(cat VERSION 2>/dev/null || echo dev)}"
UPDATE_URL="${UPDATE_URL:-}"
LDFLAGS="-s -w -X main.version=${VERSION}"
if [[ -n "$UPDATE_URL" ]]; then
  LDFLAGS="$LDFLAGS -X main.defaultUpdateURL=${UPDATE_URL}"
fi
UPDATE_SIGNING_KEY="${UPDATE_SIGNING_KEY:-}"
UPDATE_PUBLIC_KEYS="${UPDATE_PUBLIC_KEYS:-}"
if [[ -n "$UPDATE_SIGNING_KEY$UPDATE_PUBLIC_KEYS" ]]; then
  # the binaries only install releases signed by one of these keys (validated here)
  KEYARGS=()
  if [[ -n "$UPDATE_SIGNING_KEY" ]]; then KEYARGS=(-key "$UPDATE_SIGNING_KEY"); fi
  # shellcheck disable=SC2086
  KEYS="$(go run ./cmd/dumpcat release pubkey ${KEYARGS[@]+"${KEYARGS[@]}"} $UPDATE_PUBLIC_KEYS)"
  LDFLAGS="$LDFLAGS -X main.updatePublicKeys=${KEYS}"
  echo "==> trusted release keys: $KEYS"
fi
mkdir -p dist
if [[ "${SKIP_TESTS:-0}" != 1 ]]; then
  go vet ./...
  go test ./...
fi
# the packages ship THIRD_PARTY_NOTICES.md; it must match go.mod (regenerate: go run ./scripts/notices)
go run ./scripts/notices -check
for arch in amd64 arm64; do
  echo "==> linux/$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "dist/dumpcat-linux-$arch" ./cmd/dumpcat
done
PKG="dumpcat-${VERSION}-linux"
rm -rf "dist/$PKG"
mkdir -p "dist/$PKG"
cp dist/dumpcat-linux-amd64 dist/dumpcat-linux-arm64 "dist/$PKG/"
cp scripts/install.sh deploy/dumpcat.service deploy/dumpcat-agent.service deploy/dumpcat.env.example deploy/agent.env.example deploy/nginx-dumpcat.conf README.md README.vi.md CHANGELOG.md CONTRIBUTING.md SECURITY.md LICENSE NOTICE THIRD_PARTY_NOTICES.md "dist/$PKG/"
mkdir -p "dist/$PKG/docs" && cp docs/manual.md docs/manual.vi.md "dist/$PKG/docs/"
chmod +x "dist/$PKG/install.sh"
tar --owner=0 --group=0 --numeric-owner -C dist -czf "dist/$PKG.tar.gz" "$PKG"
rm -rf "dist/$PKG"
echo "==> dist/$PKG.tar.gz"

echo "==> windows/amd64"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/dumpcat-windows-amd64.exe ./cmd/dumpcat
WPKG="dumpcat-${VERSION}-windows-amd64"
rm -rf "dist/$WPKG" "dist/$WPKG.zip"
mkdir -p "dist/$WPKG"
cp dist/dumpcat-windows-amd64.exe "dist/$WPKG/dumpcat.exe"
cp scripts/install.ps1 deploy/dumpcat.env.example deploy/agent.env.example README.md README.vi.md CHANGELOG.md CONTRIBUTING.md SECURITY.md LICENSE NOTICE THIRD_PARTY_NOTICES.md "dist/$WPKG/"
mkdir -p "dist/$WPKG/docs" && cp docs/manual.md docs/manual.vi.md "dist/$WPKG/docs/"
if command -v zip >/dev/null; then
  ( cd dist && zip -qr "$WPKG.zip" "$WPKG" )
else
  PY="$(command -v python3 >/dev/null && python3 -c '' 2>/dev/null && echo python3 || echo python)"
  ( cd dist && "$PY" -m zipfile -c "$WPKG.zip" "$WPKG" )
fi
rm -rf "dist/$WPKG"
echo "==> dist/$WPKG.zip"

( cd dist && sha256sum dumpcat-linux-* dumpcat-windows-amd64.exe "$PKG.tar.gz" "$WPKG.zip" | sed 's/ \*/  /' > SHA256SUMS )

# ---- update folder: latest.json + <version>/ (upload to the release server) ----
REL="dist/update/$VERSION"
rm -rf "$REL" dist/update/latest.json.sig
mkdir -p "$REL"
cp dist/dumpcat-linux-amd64 dist/dumpcat-linux-arm64 dist/dumpcat-windows-amd64.exe "dist/$PKG.tar.gz" "dist/$WPKG.zip" "$REL/"
( cd "$REL" && sha256sum * | sed 's/ \*/  /' > SHA256SUMS )
sum()  { sha256sum "$1" | cut -d' ' -f1; }
size() { wc -c < "$1" | tr -d ' '; }
json_escape() { awk 'BEGIN { ORS = "" } { gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); gsub(/\t/, "\\t"); gsub(/\r/, ""); if (NR > 1) print "\\n"; print }'; }
# release notes: the "## <version>" section of CHANGELOG.md
NOTES="$(awk -v v="$VERSION" '/^## / { if (on) exit; on = (index($0, "## " v) == 1); next } on' CHANGELOG.md 2>/dev/null | sed -e '/./,$!d' | json_escape)"
entry() { printf '    "%s": {"url": "%s/%s", "sha256": "%s", "size": %s}' "$1" "$VERSION" "$2" "$(sum "$REL/$2")" "$(size "$REL/$2")"; }
{
  printf '{\n  "version": "%s",\n  "released_at": "%s",\n  "notes": "%s",\n  "files": {\n' "$VERSION" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$NOTES"
  entry linux/amd64 dumpcat-linux-amd64; printf ',\n'
  entry linux/arm64 dumpcat-linux-arm64; printf ',\n'
  entry windows/amd64 dumpcat-windows-amd64.exe; printf '\n'
  printf '  }\n}\n'
} > dist/update/latest.json
echo "==> dist/update/latest.json ($VERSION)"
if [[ -n "$UPDATE_SIGNING_KEY" ]]; then
  go run ./cmd/dumpcat release sign -key "$UPDATE_SIGNING_KEY" dist/update/latest.json
elif [[ -n "$UPDATE_PUBLIC_KEYS" ]]; then
  echo "WARNING: latest.json is not signed — installations that trust a key will refuse it." >&2
  echo "         Sign it before uploading: dumpcat release sign -key FILE dist/update/latest.json" >&2
fi
echo "==> upload dist/update/ to the release server (the version folder first, latest.json.sig and latest.json last)"
