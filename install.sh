#!/usr/bin/env bash
#
# install.sh — the curl installer for Chimera (macOS / Linux).
#
#   curl -fsSL https://raw.githubusercontent.com/NaMan6122/ChimerA/main/install.sh | bash
#
# Options via environment (the curl|bash form has no argv):
#   CHIMERA_VERSION=v0.4.0    install a specific release instead of the latest
#   CHIMERA_PREFIX=/opt/x     install under a custom prefix (default ~/.local)
#   CHIMERA_FROM_DIR=./dist   install from local release artifacts (no network;
#                             what `scripts/dist.sh` produces)
#
# Layout is identical to ./build.sh, so the two install paths are
# interchangeable and both upgrade in place:
#   $PREFIX/share/chimera/versions/<version>/{chimera,chimera-spin,chimera-chat}
#   $PREFIX/bin/<name>  ->  versioned binary
set -euo pipefail

REPO="NaMan6122/ChimerA"
PREFIX=${CHIMERA_PREFIX:-$HOME/.local}
FROM_DIR=${CHIMERA_FROM_DIR:-}

say()  { printf '==> %s\n' "$1"; }
fail() { printf 'error: %s\n' "$1" >&2; exit 1; }

case "$(uname -s)" in
    Darwin) goos=darwin ;;
    Linux)  goos=linux ;;
    *)      fail "unsupported OS: $(uname -s) — use the windows_amd64.zip release asset" ;;
esac
case "$(uname -m)" in
    arm64|aarch64) goarch=arm64 ;;
    x86_64|amd64)  goarch=amd64 ;;
    *)             fail "unsupported architecture: $(uname -m)" ;;
esac

sha_of() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# --- resolve which release to install ---------------------------------------
if [ -n "${CHIMERA_VERSION:-}" ]; then
    VERSION=$CHIMERA_VERSION
    say "installing pinned release $VERSION"
elif [ -n "$FROM_DIR" ]; then
    VERSION=$(ls "$FROM_DIR" 2>/dev/null | sed -n "s/^chimera_\\(.*\\)_${goos}_${goarch}\\.tar\\.gz\$/\\1/p" | head -n1)
    [ -n "$VERSION" ] || fail "no ${goos}/${goarch} artifact in $FROM_DIR — run scripts/dist.sh first"
    say "installing local build $VERSION from $FROM_DIR"
else
    say "resolving latest release"
    VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
    [ -n "$VERSION" ] || fail "could not resolve the latest release (rate limit? set CHIMERA_VERSION=v0.4.0)"
    say "installing latest release $VERSION"
fi

VERNUM=${VERSION#v}
ART="chimera_${VERNUM}_${goos}_${goarch}.tar.gz"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# --- fetch -------------------------------------------------------------------
if [ -n "$FROM_DIR" ]; then
    cp "$FROM_DIR/$ART" "$FROM_DIR/checksums.txt" "$TMP/"
else
    say "downloading $ART"
    curl -fsSL --retry 3 "https://github.com/$REPO/releases/download/$VERSION/$ART" -o "$TMP/$ART" \
        || fail "download failed — does release $VERSION exist? (CHIMERA_VERSION pins one)"
    curl -fsSL --retry 3 "https://github.com/$REPO/releases/download/$VERSION/checksums.txt" -o "$TMP/checksums.txt" \
        || fail "checksums.txt missing from release $VERSION"
fi

# --- verify ------------------------------------------------------------------
WANT=$(awk -v f="$ART" '$2==f {print $1}' "$TMP/checksums.txt")
[ -n "$WANT" ] || fail "$ART is not listed in checksums.txt — refusing to install"
GOT=$(sha_of "$TMP/$ART")
if [ "$GOT" != "$WANT" ]; then
    fail "checksum mismatch for $ART\n  want $WANT\n  got  $GOT"
fi
say "checksum verified"

# --- install -----------------------------------------------------------------
# Version-dir name matches what build.sh (git describe) produces for the same
# tag, so the two install paths upgrade over each other instead of leaving
# two trees. Artifact filenames strip the leading "v"; restore it for
# semver-looking local builds.
if [ -n "$FROM_DIR" ] && [ "${VERSION#v}" = "$VERSION" ] && echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+'; then
    VERSION="v$VERSION"
fi
DEST="$PREFIX/share/chimera/versions/$VERSION"
mkdir -p "$DEST" "$PREFIX/bin"
tar -xzf "$TMP/$ART" -C "$DEST"
for name in chimera chimera-spin chimera-chat; do
    chmod 0755 "$DEST/$name"
    ln -sfn "$DEST/$name" "$PREFIX/bin/$name"
    printf '    %s -> %s\n' "$PREFIX/bin/$name" "$DEST/$name"
done

case ":$PATH:" in
    *":$PREFIX/bin:"*) ;;
    *) printf '\n%s\n%s\n' "==> note: $PREFIX/bin is not on PATH" \
       "    export PATH=\"$PREFIX/bin:\$PATH\"   # add to your shell rc" ;;
esac

say "installed chimera $VERSION ($("$PREFIX/bin/chimera" -version))"
cat <<'EOS'

Next steps (run in the directory where you want auth_data/ to live):
    chimera auth login chatgpt          # opens a browser once, exports the session
    TRANSPORT=webapi PROVIDER=chatgpt chimera
    chimera-spin -base-url http://localhost:8000/v1 -model chimera-chatgpt "say OK"
EOS
