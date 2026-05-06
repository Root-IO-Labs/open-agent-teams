# Releasing OAT

This guide covers cutting a new release of `oat` and `oat-agent` to the GitHub Releases page and the Homebrew tap (`Root-IO-Labs/homebrew-oat`).

## How releases work

Releases are tag-driven. Pushing a tag matching `v*` to the main repository fires `.github/workflows/release.yml`, which runs [GoReleaser](https://goreleaser.com) to:

1. Cross-compile `oat` and `oat-agent` for darwin/{arm64,amd64} and linux/{arm64,amd64}.
2. Stage `agent-runtime/` (excluding venvs, `__pycache__`, etc.) via `scripts/stage-agent-runtime.sh`.
3. Build a `.tar.gz` per platform containing `oat`, `oat-agent`, the staged `agent-runtime/`, `LICENSE`, and `README.md`.
4. Generate a SHA256 `checksums.txt`.
5. Publish a GitHub release with all archives attached.
6. Auto-commit an updated `Formula/oat.rb` to the `Root-IO-Labs/homebrew-oat` tap.

The version string is embedded into both binaries via `-ldflags`, so `oat --version` and `oat-agent --version` report the tag.

Windows is not currently supported because `pkg/backend/direct_backend.go` uses Unix-only PTY syscalls. Cross-compile is verified clean for the four supported targets.

## Prerequisites (one-time setup)

These need to be in place before the first release will succeed:

1. **Both repos public** — `Root-IO-Labs/open-agent-teams` and `Root-IO-Labs/homebrew-oat`. Public repos get unlimited free Actions minutes, and `brew install` only works without auth against public repos.

2. **`HOMEBREW_TAP_GITHUB_TOKEN` secret** — GoReleaser needs to write to the *tap* repo, but the default `GITHUB_TOKEN` only has access to the repo running the workflow. Create a fine-grained personal access token:
   - https://github.com/settings/personal-access-tokens/new
   - Name: `homebrew-oat-release`
   - Resource owner: `Root-IO-Labs`
   - Repository access: *Only select repositories* → `Root-IO-Labs/homebrew-oat`
   - Repository permissions: `Contents: Read and write`
   - Add it under `Root-IO-Labs/open-agent-teams` → Settings → Secrets and variables → Actions → New repository secret named `HOMEBREW_TAP_GITHUB_TOKEN`

## Cutting a release

```bash
git checkout main
git pull
git tag -a v0.1.0 -m "v0.1.0 — first public release"
git push origin v0.1.0
```

The release workflow runs in 2–3 minutes and produces:
- A GitHub release at `v0.1.0` with 4 tarballs + `checksums.txt`
- An auto-committed `Formula/oat.rb` in the tap repo

After the workflow finishes, verify on a clean machine:

```bash
brew install Root-IO-Labs/oat/oat
oat --version       # should report v0.1.0
oat-agent --version # should report v0.1.0
oat doctor          # should report all green
```

## Versioning

We follow [Semantic Versioning](https://semver.org):

- `vMAJOR.MINOR.PATCH`
- Increment `PATCH` for bug fixes
- Increment `MINOR` for backwards-compatible features
- Increment `MAJOR` for breaking changes (e.g. on-disk state format changes that need migration, removed CLI flags)

Pre-1.0 the contract is looser, but we still try to avoid surprise breakage between minor releases.

## Local dry-run

To validate the release pipeline without publishing:

```bash
make release-check     # validates .goreleaser.yml syntax
make release-snapshot  # builds all 4 platforms locally to dist/, no publish
```

`make release-snapshot` produces the same archives the real release would, named with a `-snapshot-<sha>` suffix. Extract one and run the binary to confirm everything's wired correctly.

Snapshots are gitignored (`/dist/`, `/.release-stage/`).

## Changelog

GoReleaser auto-generates the release notes from commit messages between the previous tag and the current one, excluding commits with `docs:`, `test:`, `chore:`, or `Merge` prefixes (see `.goreleaser.yml`).

For a more curated changelog, edit the release notes on the GitHub Releases page after the workflow finishes.

## Rollback

If a release ships broken:

1. **Don't delete the tag** — it's now a public reference. Cut a new patch release with the fix.
2. If the broken version is in the Homebrew tap and actively harming users, you can manually edit `Formula/oat.rb` in the tap repo to point back to the previous version's tarball SHA, then commit. GoReleaser will overwrite this on the next release.
