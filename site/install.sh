#!/bin/sh
# DumpCat one-line installer for Linux, served at https://dumpcat.cc/install.sh
#
#   curl -fsSL https://dumpcat.cc/install.sh | sudo sh
#   curl -fsSL https://dumpcat.cc/install.sh | sudo sh -s -- --port 9090
#   curl -fsSL https://dumpcat.cc/install.sh | sudo sh -s -- --uninstall
#   curl -fsSL https://dumpcat.cc/install.sh | sudo sh -s -- --agent --server https://backup.example.com --key dca_...
#
# Downloads the latest release package from downloads.dumpcat.cc, checks its SHA-256 against the release's
# SHA256SUMS, unpacks it in a temporary folder and runs the install.sh inside it with the same options
# (see `sudo ./install.sh --help` in the package). The temporary folder is removed afterwards.
#
# DUMPCAT_VERSION=1.3.1 installs a given version; DUMPCAT_DOWNLOAD_URL points at another release server.
set -eu

main() {
  base="${DUMPCAT_DOWNLOAD_URL:-https://downloads.dumpcat.cc}"
  base="${base%/}"

  say() { printf '==> %s\n' "$*"; }
  die() { printf 'xx  %s\n' "$*" >&2; exit 1; }

  [ "$(uname -s)" = Linux ] || die "this installer is for Linux; on Windows see https://dumpcat.cc/install/"
  [ "$(id -u)" -eq 0 ] || die "please run as root: curl -fsSL https://dumpcat.cc/install.sh | sudo sh"
  command -v bash >/dev/null 2>&1 || die "bash is required"
  command -v tar >/dev/null 2>&1 || die "tar is required"
  command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required (coreutils)"
  if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
  elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
  else
    die "curl or wget is required"
  fi

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  trap 'exit 130' INT TERM

  version="${DUMPCAT_VERSION:-}"
  if [ -z "$version" ]; then
    fetch "$base/latest.json" "$tmp/latest.json" || die "cannot reach $base/latest.json"
    version="$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
    [ -n "$version" ] || die "no version in $base/latest.json"
  fi
  pkg="dumpcat-$version-linux"

  say "Downloading DumpCat $version"
  fetch "$base/$version/$pkg.tar.gz" "$tmp/$pkg.tar.gz" || die "download failed: $base/$version/$pkg.tar.gz"
  fetch "$base/$version/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $base/$version/SHA256SUMS"

  want="$(awk -v f="$pkg.tar.gz" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")"
  got="$(sha256sum "$tmp/$pkg.tar.gz" | cut -d' ' -f1)"
  [ -n "$want" ] || die "$pkg.tar.gz is not listed in SHA256SUMS"
  [ "$want" = "$got" ] || die "checksum mismatch for $pkg.tar.gz: the download is damaged, please try again"
  say "Checksum OK"

  tar -xzf "$tmp/$pkg.tar.gz" -C "$tmp"
  cd "$tmp/$pkg"
  # stdin is this script's pipe: give the installer the terminal, so its questions can be answered
  if (: </dev/tty) 2>/dev/null; then
    bash ./install.sh "$@" </dev/tty
  else
    bash ./install.sh "$@" </dev/null
  fi
}

# everything runs inside main, so a download cut short never runs half a script
main "$@"
