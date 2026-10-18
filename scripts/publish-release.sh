#!/usr/bin/env bash
# Upload dist/update/ (written by ./build.sh) to the R2 bucket behind https://downloads.dumpcat.cc.
# Order matters: the version folder first, latest.json.sig and latest.json last, so installations never
# see a release whose files are not there yet. Needs `npx wrangler login` once; see docs/website.vi.md.
#   scripts/publish-release.sh
#   BUCKET=other-bucket RELEASE_URL=https://other.example scripts/publish-release.sh
set -euo pipefail
cd "$(dirname "$0")/.."
BUCKET="${BUCKET:-dumpcat-releases}"
RELEASE_URL="${RELEASE_URL:-https://downloads.dumpcat.cc}"
U=dist/update

[[ -f $U/latest.json ]] || { echo "no $U/latest.json: run ./build.sh first" >&2; exit 1; }
V="$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$U/latest.json" | head -1)"
[[ -n $V && -d $U/$V ]] || { echo "version folder $U/$V not found" >&2; exit 1; }

# Version folders are cached for a year: never replace a published version with different files.
remote="$(curl -fsS "$RELEASE_URL/$V/SHA256SUMS" 2>/dev/null || true)"
if [[ -n $remote && $remote != "$(cat "$U/$V/SHA256SUMS")" ]]; then
  echo "$V is already published with different files; release a new version instead" >&2
  exit 1
fi
[[ -f $U/latest.json.sig ]] || echo "warning: latest.json is not signed (build with UPDATE_SIGNING_KEY)" >&2

put() { # <key under the bucket> <content type> <cache-control>
  echo "==> $BUCKET/$1"
  npx --yes wrangler r2 object put "$BUCKET/$1" --file "$U/$1" --content-type "$2" --cache-control "$3" --remote >/dev/null
}
for f in "$U/$V"/*; do
  n="$(basename "$f")"
  case "$n" in
    *.tar.gz)   type=application/gzip ;;
    *.zip)      type=application/zip ;;
    SHA256SUMS) type="text/plain; charset=utf-8" ;;
    *)          type=application/octet-stream ;;
  esac
  put "$V/$n" "$type" "public, max-age=31536000, immutable"
done
[[ -f $U/latest.json.sig ]] && put latest.json.sig "text/plain; charset=utf-8" no-cache
put latest.json application/json no-cache
echo "==> published $V: $RELEASE_URL/latest.json"
