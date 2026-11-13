# Distribution — Homebrew, curl, and release cuts

How Chimera reaches users as prebuilt binaries. Everything here is generated
from two files: [`scripts/dist.sh`](../scripts/dist.sh) (build + package) and
[`install.sh`](../install.sh) (the curl installer). CI runs the first
(`.github/workflows/release.yml`); users run the second.

The unit of distribution is one archive per platform containing **all three
binaries** — `chimera` (gateway), `chimera-spin` (headless harness),
`chimera-chat` (TUI) — plus `LICENSE` and `README.md`. They share one repo and
one version; they are not split across packages.

| Artifact | Platforms | Consumer |
|---|---|---|
| `chimera_<ver>_<os>_<arch>.tar.gz` | darwin, linux × amd64, arm64 | `install.sh`, the Homebrew formula |
| `chimera_<ver>_windows_amd64.zip` | windows | manual download (no installer) |
| `checksums.txt` | all | sha256 verification, mandatory in `install.sh` |
| `chimera.rb` | all | copied into the Homebrew tap |

All builds are `CGO_ENABLED=0 -trimpath`, ldflags-stamped with
`main.version=<tag>`, so `chimera -version` reports the release tag and the
binaries are static.

---

## 1. The user-facing surfaces

**curl (macOS / Linux):**

```bash
curl -fsSL https://raw.githubusercontent.com/NaMan6122/ChimerA/main/install.sh | bash
CHIMERA_VERSION=v0.4.0 curl -fsSL .../install.sh | bash   # pin a release
```

The installer is self-documenting: it prints the full guide (what it does,
manual transport setup, agent-driven setup) after every install and on
`install.sh --help`; `install.sh --agent` prints a paste-able prompt for a
coding agent that deliberately leaves only the browser login to the human.
It detects
OS/arch, resolves the latest GitHub release, downloads the tarball, verifies
it against `checksums.txt` (refuses on mismatch), and installs the same
layout as `./build.sh`:

```
~/.local/share/chimera/versions/v0.4.0/{chimera,chimera-spin,chimera-chat}
~/.local/bin/{chimera,chimera-spin,chimera-chat}  ->  versioned binaries
~/.local/bin/chimera-{chatgpt,gpt,qwen,deepseek,kimi,gemini,grok,glm}
                                                  ->  chimera (one-word `up`)
```

The `chimera-<provider>` names are symlinks to the gateway, which dispatches on
its own `argv[0]`; they are created by `install.sh` and `./build.sh`, so the
tarball still ships exactly three binaries.

Because the layout is identical, `install.sh` and `./build.sh` can upgrade over
each other. `CHIMERA_PREFIX` relocates it; `CHIMERA_FROM_DIR=./dist` installs
from local artifacts with no network (how the installer is tested in CI-less
environments).

**Homebrew:**

```bash
brew tap NaMan6122/chimera
brew trust naman6122/chimera   # once per machine (Homebrew 4.6+)
brew install chimera
```

## 2. Cutting a release

```bash
git tag v0.4.0 && git push origin v0.4.0
```

That is the whole run. The workflow builds every platform on the tag commit,
creates the GitHub release with `--generate-notes`, and uploads the five
archives, `checksums.txt`, and `chimera.rb`. Watch it under
<https://github.com/NaMan6122/ChimerA/actions>.

Tag rules:

- Tags are `v`-prefixed semver (`v0.4.0`). `dist.sh` warns and the formula's
  download URL breaks otherwise — filenames strip the `v`, release URLs keep it.
- `CHANGELOG.md` is updated in the same commit series as the tag — release
  notes auto-generate from commits; the changelog carries the curated story.
- One tag = immutable release. To fix a packaging bug, cut `v0.4.1`; never
  re-push a tag (Homebrew's sha256 pinning would silently disagree with the
  archive it points at).
- Before tagging, run the local dry run (§4) so the CI build is a formality.

## 3. The Homebrew tap (one-time setup, then one copy per release)

Homebrew installs from a *tap* — a separate repository holding the formula.
The formula itself is generated per release (`dist/chimera.rb`) and pins the
sha256 of each platform archive, with `on_macos`/`on_linux` blocks so one
formula serves Intel/Apple Silicon/x86/ARM64 Linux.

One-time:

```bash
# create github.com/NaMan6122/homebrew-chimera (empty, public), then:
git clone git@github.com:NaMan6122/homebrew-chimera.git
cd homebrew-chimera && mkdir Formula
```

Per release (after the workflow goes green):

```bash
cd /path/to/homebrew-chimera
curl -fsSLo Formula/chimera.rb \
  https://github.com/NaMan6122/ChimerA/releases/download/v0.4.0/chimera.rb
git add Formula/chimera.rb && git commit -m "chimera 0.4.0" && git push
brew audit --formula NaMan6122/chimera/chimera   # optional but cheap
```

`livecheck` in the formula uses `strategy :github_latest`, so
`brew livecheck chimera` reports staleness against the newest release.

The formula's `test do` runs all three binaries' `-version` and asserts the
installed version matches — `brew test chimera` exercises it locally.

## 4. Verifying a release locally (before tagging)

```bash
CHIMERA_VERSION=v0.4.0-test ./scripts/dist.sh      # full cross-compile, ~1 min
( cd dist && shasum -a 256 -c checksums.txt )       # integrity of the set
tar -tzf dist/chimera_0.4.0-test_darwin_arm64.tar.gz
CHIMERA_FROM_DIR=dist CHIMERA_PREFIX=/tmp/chimera-test ./install.sh
/tmp/chimera-test/bin/chimera -version              # all three report the stamp
ruby -c dist/chimera.rb                             # formula syntax
```

`dist/` is a build product: gitignored, rebuilt from scratch on every run.

## 5. Why not gorereleaser / why not homebrew-core

- **Hand-rolled `dist.sh`** — three binaries, five artifacts, no brew bottle
  publishing: ~120 lines of shell with zero new dependencies, reviewable
  end-to-end, and it runs on a laptop as well as CI. goreleaser buys almost
  nothing here and adds a config dialect.
- **A tap, not homebrew-core** — homebrew-core requires notable popularity and
  bottles built by Homebrew's CI. A tap ships the same static binaries our
  release already builds, updates with one file copy, and has no admission
  process. If the project grows, migrating the formula to core is mechanical.
- **Windows gets an archive, not an installer** — the DOM substrate wants a
  real desktop session and the two install surfaces (brew, POSIX shell) are
  unix-shaped. Windows users download the zip; the binaries themselves are
  identical Go code.

## 6. Notes for agents touching this

- `install.sh` is executed via `curl | bash`: env vars plus two flags
  (`--help`, `--agent`, passed as `bash -s -- --agent`); `set -euo pipefail`;
  no `jq`/`gh` dependency — only `curl`, `awk`, `sed`, `tar`,
  `shasum`/`sha256sum`.
- The agent prompt in `--agent` must keep telling the agent NOT to run
  `chimera auth login`/`verify`: those open a real browser (and a GUI browser
  launched from an agent shell gets reaped), and captcha automation is
  against project policy. If that split is ever wrong, the installer is advertising
  something the product forbids.
- `checksums.txt` lines are `<sha256>  <name>` with the name relative to the
  release root; both `dist.sh` (writer) and `install.sh` (verifier) and the
  formula (reader via `sha_for`) must agree on that shape.
- Cross-compilation is only safe while `CGO_ENABLED=0` holds. `modernc.org/
  sqlite` is the dependency that would break this if it were swapped for
  `mattn/go-sqlite3` — don't.
- The gateway binary is not enough by itself for a new user: `auth_data/` is
  created by `chimera auth login` in the *working directory*. Both install
  paths print this; keep it that way.
