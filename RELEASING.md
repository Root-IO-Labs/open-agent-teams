# Releasing OAT

This document describes how to cut a release of OAT. Releases produce
pre-built binaries for macOS and Linux on both x86_64 and arm64, plus a
bundled Python `agent-runtime/` source tree, attached to a GitHub Release.

## Overview

- **Trigger**: pushing a `v*` tag to the repo (e.g. `v0.1.0`).
- **Driver**: [GoReleaser](https://goreleaser.com/) v2 via
  `.github/workflows/release.yml`.
- **Output**: one tarball per platform at
  `https://github.com/Root-IO-Labs/open-agent-teams/releases/tag/<tag>`,
  plus `checksums.txt`.

## What gets built

`.goreleaser.yml` defines two builds:

| Build | Source | Notes |
|-------|--------|-------|
| `oat` | `cmd/oat` | Version + Commit + Date stamped via `-ldflags '-X .../internal/version.{Version,Commit,Date}=<…>'` |
| `oat-agent` | `cmd/oat-agent` | Same ldflags stamping; static build (CGO disabled) |

Each archive is named `oat_<version>_<Os>_<arch>.tar.gz` (capitalized OS to match goreleaser's `{{ title .Os }}` template — e.g. `oat_0.1.0_Darwin_arm64.tar.gz`) and contains:

```
oat_<version>_<Os>_<arch>/
├── oat                    # Go binary (version-stamped)
├── oat-agent              # Go binary (version-stamped)
├── agent-runtime/         # Python source for the agent runtime
│                          # (staged via scripts/stage-agent-runtime.sh —
│                          #  excludes venvs, pycache, examples, partners)
├── install.sh             # bundled installer (= scripts/install-from-release.sh)
├── LICENSE
├── README.md
├── CHANGELOG.md
├── CONTRIBUTING.md
└── SECURITY.md
```

`install.sh` performs the post-extract setup: stages files into
`~/.oat/install/`, symlinks `oat`/`oat-agent` into `~/.local/bin/`, and
runs `uv sync` inside `agent-runtime/libs/cli/`.

## Cutting a release

### 1. Pre-flight on `main`

```bash
git checkout main
git pull
make check-all          # build + unit + e2e + verify-docs + coverage
go vet ./...
```

If anything fails, fix it on `dev` first and merge into `main` per
`CONTRIBUTING.md`.

### 2. Pick a version

Use semver: `v<major>.<minor>.<patch>`. Conventional starting point is
`v0.1.0` for the first public release.

### 3. (Optional) Local dry run

GoReleaser can build everything locally without publishing — useful for
validating the config or inspecting an archive before tagging.

```bash
brew install goreleaser            # one-time, macOS
goreleaser release --snapshot --clean
ls dist/                            # archives, checksums, metadata
tar -tzf dist/oat_*_Darwin_arm64.tar.gz | head -20
```

`--snapshot` skips git tag validation and builds with a synthetic
version. `dist/` is gitignored.

### 4. Tag and push

```bash
git tag -a v0.1.0 -m "v0.1.0 — initial public release"
git push origin v0.1.0
```

Pushing the tag triggers `.github/workflows/release.yml`. Watch it run:

```bash
gh run watch
```

The workflow will:

1. Check out the repo at the tagged commit.
2. Set up Go 1.25.
3. Run `goreleaser release --clean`, which:
   - cross-compiles `oat` and `oat-agent` for the four target platforms,
   - assembles each archive (binaries + agent-runtime + install.sh + license),
   - creates a GitHub Release with the archives and `checksums.txt`,
   - generates release notes from commit messages (see
     `.goreleaser.yml#changelog` for the grouping rules).

### 5. Verify the release

Once the workflow succeeds:

```bash
gh release view v0.1.0
gh release view v0.1.0 --json assets --jq '.assets[].name'
```

You should see four `oat_<version>_<Os>_<arch>.tar.gz` files plus
`checksums.txt`.

Test the install end-to-end on a clean machine (or at least a fresh
shell):

```bash
curl -fsSL https://raw.githubusercontent.com/Root-IO-Labs/open-agent-teams/main/install.sh | bash
oat version          # should print: oat v0.1.0
```

### 6. Announce

Update any external references (Discord, blog, social) once the release
page looks correct.

## Pinning to a specific version

The one-line installer respects an `OAT_VERSION` env var:

```bash
OAT_VERSION=v0.1.0 \
  curl -fsSL https://raw.githubusercontent.com/Root-IO-Labs/open-agent-teams/main/install.sh | bash
```

## Troubleshooting

- **Workflow fails at `goreleaser release --clean`**: look at the
  workflow logs. Common causes: dirty working tree on the tagged
  commit, broken `go mod tidy`, or a build that fails on one of the
  cross-compiled platforms (run `goreleaser release --snapshot --clean`
  locally to reproduce).
- **Archive missing files**: edit `archives.files` in `.goreleaser.yml`.
  Globs are evaluated relative to the repo root.
- **Version not stamped**: confirm the `-ldflags` paths in
  `.goreleaser.yml` match the package path of `Version` / `Commit` /
  `Date` in `internal/version/version.go`.
- **Re-cutting a tag**: GitHub Releases reject duplicate tag pushes.
  Delete the tag locally and remotely, fix, then re-tag with the
  same name.

  ```bash
  git tag -d v0.1.0
  git push origin :refs/tags/v0.1.0
  gh release delete v0.1.0 --yes   # if a partial release got created
  ```

## Release cadence

We don't have a fixed cadence yet. Cut a release when:

- A user-visible feature lands on `main`, or
- A meaningful bug fix lands on `main`, or
- An external contributor needs the change tagged for downstream use.

Patch (`vX.Y.Z+1`) for bugfix-only, minor (`vX.Y+1.0`) for new features
or workflow changes, major (`vX+1.0.0`) for breaking changes to the
CLI surface or state-file schema.
