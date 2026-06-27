#!/bin/sh
# install.sh — one-line installer for the atlas binary (M4, docs/internal/m4-build-plan.md).
#
#   curl -fsSL https://raw.githubusercontent.com/orchestra-hq/atlas/main/install.sh | sh
#
# Detects OS/arch, downloads the matching signed release archive from GitHub
# Releases, verifies its sha256 against checksums.txt (and the cosign signature
# when cosign is installed), and installs `atlas` onto PATH. Non-interactive and
# idempotent. Honors env overrides:
#
#   ATLAS_VERSION      version to install, e.g. v0.2.0 (default: latest release)
#   ATLAS_INSTALL_DIR  install target (default: /usr/local/bin, else ~/.local/bin)
#   ATLAS_REPO         owner/repo (default: orchestra-hq/atlas)
#   ATLAS_BASE_URL     override the asset directory URL (testing; skips the API,
#                      requires ATLAS_VERSION)
#   GITHUB_TOKEN       bearer token for the API + downloads (private-repo testing)
#
# POSIX sh; needs curl or wget, tar, and sha256sum or shasum.
set -eu

REPO="${ATLAS_REPO:-orchestra-hq/atlas}"

die() { echo "atlas-install: $*" >&2; exit 1; }
info() { echo "atlas-install: $*"; }

# --- prerequisites -----------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  api_get() { curl -fsSL ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} "$1"; }
  fetch() { curl -fsSL ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  api_get() { wget -qO- ${GITHUB_TOKEN:+--header="Authorization: Bearer $GITHUB_TOKEN"} "$1"; }
  fetch() { wget -qO "$2" ${GITHUB_TOKEN:+--header="Authorization: Bearer $GITHUB_TOKEN"} "$1"; }
else
  die "need curl or wget"
fi
command -v tar >/dev/null 2>&1 || die "need tar"
if command -v sha256sum >/dev/null 2>&1; then
  SHA() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  SHA() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "need sha256sum or shasum"
fi

# --- detect OS/arch (GoReleaser's .Os/.Arch values) --------------------------
os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS: $os (atlas ships linux and darwin)" ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch (atlas ships amd64 and arm64)" ;;
esac

# --- resolve version + asset URLs --------------------------------------------
# GoReleaser archive: atlas_<version-without-v>_<os>_<arch>.tar.gz (see .goreleaser.yaml).
if [ -n "${ATLAS_BASE_URL:-}" ]; then
  [ -n "${ATLAS_VERSION:-}" ] || die "ATLAS_BASE_URL set but ATLAS_VERSION is not"
  tag="$ATLAS_VERSION"
  base="$ATLAS_BASE_URL"
else
  tag="${ATLAS_VERSION:-}"
  if [ -z "$tag" ]; then
    info "resolving latest release of $REPO"
    tag=$(api_get "https://api.github.com/repos/$REPO/releases/latest" \
      | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/') \
      || die "could not query the latest release (private repo? set GITHUB_TOKEN)"
    [ -n "$tag" ] || die "no published release found for $REPO"
  fi
  base="https://github.com/$REPO/releases/download/$tag"
fi
version=${tag#v} # strip leading v for the archive name
archive="atlas_${version}_${os}_${arch}.tar.gz"

info "installing atlas $tag ($os/$arch)"

# --- download + verify -------------------------------------------------------
tmp=$(mktemp -d "${TMPDIR:-/tmp}/atlas-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fetch "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "could not fetch checksums.txt"

want=$(grep " ${archive}\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || die "checksums.txt has no entry for $archive"
got=$(SHA "$tmp/$archive")
[ "$want" = "$got" ] || die "checksum mismatch for $archive (want $want, got $got)"
info "checksum verified"

# Cosign signature: best-effort. Verified when cosign is installed and the release
# carries the signature assets; otherwise the sha256 above is the integrity gate.
if command -v cosign >/dev/null 2>&1; then
  if fetch "$base/checksums.txt.pem" "$tmp/checksums.txt.pem" 2>/dev/null \
     && fetch "$base/checksums.txt.sig" "$tmp/checksums.txt.sig" 2>/dev/null; then
    if cosign verify-blob \
         --certificate "$tmp/checksums.txt.pem" \
         --signature "$tmp/checksums.txt.sig" \
         --certificate-identity-regexp "https://github.com/$REPO" \
         --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
         "$tmp/checksums.txt" >/dev/null 2>&1; then
      info "cosign signature verified"
    else
      die "cosign signature verification FAILED for checksums.txt"
    fi
  fi
fi

# --- install -----------------------------------------------------------------
tar -xzf "$tmp/$archive" -C "$tmp" atlas || die "could not extract atlas from $archive"
[ -f "$tmp/atlas" ] || die "archive did not contain an atlas binary"
chmod +x "$tmp/atlas"

if [ -n "${ATLAS_INSTALL_DIR:-}" ]; then
  dir="$ATLAS_INSTALL_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  dir=/usr/local/bin
else
  dir="$HOME/.local/bin"
fi
mkdir -p "$dir" 2>/dev/null || die "cannot create install dir $dir"

if [ -w "$dir" ]; then
  mv "$tmp/atlas" "$dir/atlas"
elif command -v sudo >/dev/null 2>&1; then
  info "installing to $dir (needs sudo)"
  sudo mv "$tmp/atlas" "$dir/atlas"
else
  die "install dir $dir is not writable and sudo is unavailable (set ATLAS_INSTALL_DIR)"
fi

info "installed atlas to $dir/atlas"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) info "note: $dir is not on your PATH — add it, e.g.  export PATH=\"$dir:\$PATH\"" ;;
esac
info "run 'atlas --help' to get started"
