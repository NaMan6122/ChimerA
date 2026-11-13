#!/usr/bin/env bash
#
# install.sh — the curl installer for Chimera (macOS / Linux).
#
#   curl -fsSL https://raw.githubusercontent.com/NaMan6122/ChimerA/main/install.sh | bash
#
# The script is self-documenting:
#   curl -fsSL .../install.sh | bash -s -- --help     full guide (what it does,
#                                                     manual transport setup,
#                                                     agent-driven setup)
#   curl -fsSL .../install.sh | bash -s -- --agent    just the paste-able
#                                                     prompt for a coding agent
#
# Options via environment (the curl|bash form has no argv besides these flags):
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

guide_usage() { cat <<'GUIDE'
Chimera installer — what it does, and what to do next
======================================================

WHAT THIS SCRIPT DOES (nothing else)
  1. Detects your OS and architecture (macOS / Linux, amd64 / arm64).
  2. Resolves the release to install: $CHIMERA_VERSION if set, else the
     latest GitHub release.
  3. Downloads chimera_<ver>_<os>_<arch>.tar.gz — ONE archive holding all
     three binaries (chimera gateway, chimera-spin harness, chimera-chat TUI)
     plus LICENSE and README.md.
  4. Verifies the archive's sha256 against the release's checksums.txt and
     refuses to install on any mismatch. This step is mandatory; it is never
     skipped.
  5. Unpacks into the same versioned layout ./build.sh uses, so source and
     curl installs upgrade over each other:

         $PREFIX/share/chimera/versions/<version>/   the binaries (real files)
         $PREFIX/bin/chimera chimera-spin chimera-chat   symlinks on PATH

  It does not touch anything else: no root, no launchd/systemd, no config
  files, no browser. Uninstall = delete the symlinks in $PREFIX/bin and the
  version dir in $PREFIX/share/chimera.

OPTIONS
  CHIMERA_VERSION=v0.4.0  pin a release (default: latest)
  CHIMERA_PREFIX=/opt/x   relocate the install (default: ~/.local)
  CHIMERA_FROM_DIR=dist/  install from local artifacts instead of the network
  --help                  this guide
  --agent                 print only the copy-paste prompt for a coding agent

  Pipe form sets env on the bash side, not curl's:
      curl -fsSL https://raw.githubusercontent.com/NaMan6122/ChimerA/main/install.sh \
        | CHIMERA_VERSION=v0.4.0 bash

AFTER INSTALLING — SET UP A TRANSPORT (manually)

  Chimera turns one chat subscription into one OpenAI-compatible endpoint.
  Each provider can be reached two ways: TRANSPORT=webapi replays the
  vendor's own HTTP API from an exported session (no browser, ~1-2 s/turn);
  TRANSPORT=dom drives the real vendor UI in Chromium (10-20 s/turn, harder
  to detect). Sessions and logs are read from the working directory, so pick
  one directory to run from — e.g. ~/chimera.

  1. Export a session once (opens a real browser; complete the login there):

         cd ~/chimera
         chimera auth login chatgpt        # or: deepseek, qwen
         chimera auth status               # validity + expiry, never secrets

     The session lands in auth_data/<provider>.json (mode 0600). It expires
     (days to weeks); re-run login when /v1/health/providers or auth status
     says so. Qwen may additionally need one human captcha clear:
     `chimera auth verify qwen`.

  2. Start the gateway:

         chimera up                  # every backend you have a session for
         chimera up kimi             # just one
         chimera-kimi                # same thing, one word (chimera-gpt, …)

     `chimera up` launches one gateway per provider on the first free port in
     8000-8011 and supervises them; Ctrl-C stops all of them. `chimera-chat`
     then discovers every backend automatically. The explicit form still
     works when you want control:

         TRANSPORT=webapi PROVIDER=chatgpt API_PORT=8000 API_TOKEN=chimera chimera

     TRANSPORT=dom drives the browser substrate instead (a Chromium window
     opens — log in once, the profile persists in ./browser_data); `claude`
     is dom-only today. Leave API_TOKEN unset to run without auth.

  3. Check it and drive it:

         curl -s http://localhost:8000/health
         curl -s -H "Authorization: Bearer chimera" http://localhost:8000/v1/models
         chimera-spin -base-url http://localhost:8000/v1 -model chimera-chatgpt \
           -token chimera "Read README.md and count the sections."
         chimera-chat -list-backends        # TUI; probes local gateways :8000-8011

  4. Point any OpenAI SDK at it: base_url http://localhost:8000/v1,
     api_key = your API_TOKEN. That is the whole integration.

HAVING AN AGENT DO IT

  Every step above is scriptable EXCEPT the browser login: `chimera auth
  login` and `auth verify` open a real browser window and need a human at
  the keyboard — and this project does not automate vendor captchas. Run
  those two yourself in your own terminal; hand everything else to your
  coding agent. `install.sh --agent` prints a ready-made prompt that encodes
  exactly this split, including the rule that the agent must stop and ask
  you when a session is expired.

FULL DOCUMENTATION
  README.md (shipped in every archive), docs/DISTRIBUTION.md (release and
  packaging internals), docs/API.md (endpoint shapes).
GUIDE
}

agent_guide() { cat <<'AGENT'
Paste the block below to your coding agent (Anv, Claude Code, Codex, ...).
It installs Chimera, starts a browserless gateway, and smoke-tests it — and
it tells the agent to stop and hand the browser login back to you.

---8<-----------------------------------------------------------------------
Set up Chimera (github.com/NaMan6122/ChimerA) on this machine from its
GitHub release binaries.

1. Install with the project's own installer, then verify:
     curl -fsSL https://raw.githubusercontent.com/NaMan6122/ChimerA/main/install.sh | bash
     chimera -version && chimera-spin -version && chimera-chat -version
   If ~/.local/bin is not on PATH, add it to my shell rc.

2. Create a workspace directory ~/chimera and work from there — the gateway
   reads auth_data/ and writes logs/ relative to the current directory.

3. Run `chimera auth status` for chatgpt, deepseek and qwen and report the
   result. Missing or expired sessions are expected on first setup; do not
   try to fix that yourself yet.

4. Start the gateway in the background from ~/chimera:
     TRANSPORT=webapi PROVIDER=chatgpt API_PORT=8000 API_TOKEN=chimera \
       nohup chimera >logs/gateway.log 2>&1 &
   Poll http://localhost:8000/health until it returns 200 (should be < 1 s),
   then GET /v1/models with "Authorization: Bearer chimera" and show me the
   model list.

5. Smoke-test through the whole stack:
     chimera-spin -base-url http://localhost:8000/v1 -model chimera-chatgpt \
       -token chimera "Reply with the single word OK"
   Show me the answer and the per-round stats on stderr.

6. If /v1/health/providers reports the chatgpt session expired or login is
   missing, STOP and print exactly this instruction for me:
     "Run `chimera auth login chatgpt` in your own terminal, complete the
      login in the browser window that opens, then tell me to continue."
   Do NOT run `chimera auth login` or `chimera auth verify` yourself, do not
   run them headless, and do not attempt to solve vendor captchas — those
   steps need a human at a real browser by design. Everything else (install,
   gateway lifecycle, health checks, probes, port changes, a second provider
   on another port) is yours to do without asking.

7. When done, summarize: installed version, gateway port, providers healthy,
   smoke-test result, and where the logs are.
--->8-----------------------------------------------------------------------
AGENT
}

# --- flags (work under `bash install.sh` and `curl | bash -s -- <flag>`) ----
for arg in "$@"; do
    case "$arg" in
        -h|--help|help) guide_usage; exit 0 ;;
        --agent)        agent_guide; exit 0 ;;
        *) fail "unknown argument: $arg (try --help)" ;;
    esac
done

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

# Version-dir name matches what build.sh (git describe) produces for the same
# tag, so the two install paths upgrade over each other instead of leaving
# two trees. Artifact filenames strip the leading "v"; restore it for
# semver-looking local builds.
if [ -n "$FROM_DIR" ] && [ "${VERSION#v}" = "$VERSION" ] && echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+'; then
    VERSION="v$VERSION"
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
DEST="$PREFIX/share/chimera/versions/$VERSION"
mkdir -p "$DEST" "$PREFIX/bin"
tar -xzf "$TMP/$ART" -C "$DEST"
for name in chimera chimera-spin chimera-chat; do
    chmod 0755 "$DEST/$name"
    ln -sfn "$DEST/$name" "$PREFIX/bin/$name"
    printf '    %s -> %s\n' "$PREFIX/bin/$name" "$DEST/$name"
done
# One-word backend launchers: `chimera-kimi` == `chimera up kimi`.
for prov in chatgpt gpt qwen deepseek kimi gemini grok glm; do
    ln -sfn "$DEST/chimera" "$PREFIX/bin/chimera-$prov"
done
printf '    %s/chimera-{chatgpt,gpt,qwen,deepseek,kimi,gemini,grok,glm} -> chimera\n' "$PREFIX/bin"

case ":$PATH:" in
    *":$PREFIX/bin:"*) ;;
    *) printf '\n%s\n%s\n' "==> note: $PREFIX/bin is not on PATH" \
       "    export PATH=\"$PREFIX/bin:\$PATH\"   # add to your shell rc" ;;
esac

say "installed chimera $VERSION ($("$PREFIX/bin/chimera" -version))"
# The full guide prints on every install: curl|bash users never see a man
# page, so the instructions travel with the action. --help prints the same
# text on demand (or before deciding to install).
guide_usage
