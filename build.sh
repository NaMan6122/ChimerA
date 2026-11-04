#!/usr/bin/env bash
#
# build.sh — build Chimera (gateway) and chimera-spin (harness), then install
# them the way anv is installed: a versioned directory plus a stable symlink on
# PATH.
#
#   ./build.sh                 build + install into ~/.local
#   ./build.sh --no-install    build into ./bin only
#   ./build.sh --prefix=/opt   install into a different prefix
#
# Layout:
#   <prefix>/share/chimera/versions/<version>/chimera
#   <prefix>/share/chimera/versions/<version>/chimera-spin
#   <prefix>/bin/chimera        -> the versioned binary
#   <prefix>/bin/chimera-spin   -> the versioned binary
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PREFIX=${PREFIX:-$HOME/.local}
INSTALL=1

for arg in "$@"; do
    case "$arg" in
        --no-install) INSTALL=0 ;;
        --prefix=*)   PREFIX="${arg#*=}" ;;
        -h|--help)    sed -n '3,19p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)            echo "unknown argument: $arg" >&2; exit 2 ;;
    esac
done

VERSION=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || date +%Y%m%d-%H%M)
LDFLAGS="-s -w -X main.version=$VERSION"

echo "==> chimera build: version=$VERSION cgo=off"
mkdir -p "$ROOT/bin"
CGO_ENABLED=0 go build -C "$ROOT" -ldflags "$LDFLAGS" -o "$ROOT/bin/chimera" ./cmd/chimera
CGO_ENABLED=0 go build -C "$ROOT" -ldflags "$LDFLAGS" -o "$ROOT/bin/chimera-spin" ./cmd/chimera-spin

if [ "$INSTALL" -eq 0 ]; then
    echo "==> built (not installed):"
    ls -lh "$ROOT/bin/chimera" "$ROOT/bin/chimera-spin" | awk '{print "    "$9" ("$5")"}'
    exit 0
fi

DEST="$PREFIX/share/chimera/versions/$VERSION"
BIN="$PREFIX/bin"
mkdir -p "$DEST" "$BIN"
install -m 0755 "$ROOT/bin/chimera" "$DEST/chimera"
install -m 0755 "$ROOT/bin/chimera-spin" "$DEST/chimera-spin"
ln -sfn "$DEST/chimera" "$BIN/chimera"
ln -sfn "$DEST/chimera-spin" "$BIN/chimera-spin"

echo "==> installed"
printf '    %s -> %s\n' "$BIN/chimera" "$(readlink "$BIN/chimera")"
printf '    %s -> %s\n' "$BIN/chimera-spin" "$(readlink "$BIN/chimera-spin")"

case ":$PATH:" in
    *":$BIN:"*) ;;
    *) echo "==> warning: $BIN is not on PATH"; echo "    export PATH=\"$BIN:\$PATH\"" ;;
esac

echo "==> smoketest"
"$BIN/chimera-spin" -version
echo
echo "next:"
echo "    TRANSPORT=webapi PROVIDER=chatgpt chimera      # one terminal"
echo "    chimera-spin \"read internal/api/server.go and summarise buildPrompt\""
