#!/usr/bin/env bash
#
# build.sh — build and install Chimera's two independent programs.
#
#   ./build.sh                build + install everything
#   ./build.sh spin           headless harness only (no backend needed to build)
#   ./build.sh chat           interactive TUI only
#   ./build.sh gateway        the gateway only
#   ./build.sh spin --no-install
#   ./build.sh --prefix=/opt spin
#
# The harness has no compile-time or runtime dependency on the gateway: it is
# stdlib-only and talks to any OpenAI-compatible endpoint. They ship together
# here only because they live in one repository.
#
# Install layout (same shape as anv): a versioned directory plus a stable
# symlink on PATH.
#   <prefix>/share/chimera/versions/<version>/<binary>
#   <prefix>/bin/<binary>
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PREFIX=${PREFIX:-$HOME/.local}
INSTALL=1
TARGET=all

for arg in "$@"; do
    case "$arg" in
        all|gateway|spin|chat) TARGET="$arg" ;;
        --no-install)     INSTALL=0 ;;
        --prefix=*)       PREFIX="${arg#*=}" ;;
        -h|--help)        sed -n '3,21p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)                echo "unknown argument: $arg (expected: all|gateway|spin)" >&2; exit 2 ;;
    esac
done

VERSION=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || date +%Y%m%d-%H%M)
LDFLAGS="-s -w -X main.version=$VERSION"
BIN="$PREFIX/bin"
DEST="$PREFIX/share/chimera/versions/$VERSION"

mkdir -p "$ROOT/bin"
BUILT=()

build() {
    local name=$1 pkg=$2
    CGO_ENABLED=0 go build -C "$ROOT" -ldflags "$LDFLAGS" -o "$ROOT/bin/$name" "$pkg"
    BUILT+=("$name")
    printf '    built %s (%s)\n' "$name" "$(du -h "$ROOT/bin/$name" | cut -f1)"
}

echo "==> chimera build: target=$TARGET version=$VERSION cgo=off"
case "$TARGET" in
    all)
        build chimera ./cmd/chimera
        build chimera-spin ./cmd/chimera-spin
        build chimera-chat ./cmd/chimera-chat
        ;;
    gateway) build chimera ./cmd/chimera ;;
    spin)    build chimera-spin ./cmd/chimera-spin ;;
    chat)    build chimera-chat ./cmd/chimera-chat ;;
esac

if [ "$INSTALL" -eq 0 ]; then
    echo "==> not installed (built into $ROOT/bin)"
    exit 0
fi

mkdir -p "$DEST" "$BIN"
for name in "${BUILT[@]}"; do
    install -m 0755 "$ROOT/bin/$name" "$DEST/$name"
    ln -sfn "$DEST/$name" "$BIN/$name"
    printf '    %s -> %s\n' "$BIN/$name" "$(readlink "$BIN/$name")"
done

# One-word backend launchers: `chimera-kimi` == `chimera up kimi`. They are
# symlinks to the gateway, which dispatches on its own argv[0].
if [ -x "$DEST/chimera" ]; then
    for prov in chatgpt gpt qwen deepseek kimi gemini grok glm; do
        ln -sfn "$DEST/chimera" "$BIN/chimera-$prov"
    done
    printf '    %s/chimera-{chatgpt,gpt,qwen,deepseek,kimi,gemini,grok,glm} -> chimera\n' "$BIN"
fi

case ":$PATH:" in
    *":$BIN:"*) ;;
    *) echo "==> warning: $BIN is not on PATH"; echo "    export PATH=\"$BIN:\$PATH\"" ;;
esac

echo "==> smoketest"
for name in "${BUILT[@]}"; do "$BIN/$name" -version; done

echo
echo "The harness is a client: point it at any running gateway."
echo "    chimera-spin -base-url http://host:8000/v1 \"<task>\""
