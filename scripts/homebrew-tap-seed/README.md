# Homebrew tap seed files

One-shot staging area for the initial commit to
[`Root-IO-Labs/homebrew-oat`](https://github.com/Root-IO-Labs/homebrew-oat).
After the tap is seeded, this whole directory can be deleted — goreleaser
owns all subsequent updates via the `brews:` block in `.goreleaser.yml`.

## Layout

- `tap/` — copy the contents of this subdirectory verbatim into the root
  of `homebrew-oat` for the initial commit. The subdirectory name is NOT
  copied; only what's inside it.

## How to seed the tap (one-time)

Run from a terminal whose `gh` auth has write access to the
`Root-IO-Labs` org. The current repo root is referred to as `$OAT_REPO`.

```bash
# 1. Clone the empty tap
gh repo clone Root-IO-Labs/homebrew-oat /tmp/homebrew-oat
cd /tmp/homebrew-oat

# 2. Copy the seed files over (contents of scripts/homebrew-tap-seed/tap/
#    into the tap repo root)
cp -R "$OAT_REPO/scripts/homebrew-tap-seed/tap/." .

# 3. Commit + push
git add .
git commit -m "chore: initial tap scaffolding

Seed formula + README + LICENSE + .gitignore. The formula is a stub that
fails fast with an 'install from source' message until goreleaser
overwrites it on the first v* tag of Root-IO-Labs/open-agent-teams."
git push origin main

# 4. Set description + topics on the tap repo
gh api -X PATCH /repos/Root-IO-Labs/homebrew-oat \
  -f description='Official Homebrew tap for OAT (Open Agent Teams) — orchestrate parallel coding agents on GitHub repos. Auto-updated by GoReleaser on each tagged release of Root-IO-Labs/open-agent-teams.' \
  -f homepage='https://github.com/Root-IO-Labs/open-agent-teams'

gh api -X PUT /repos/Root-IO-Labs/homebrew-oat/topics \
  -f 'names[]=homebrew' \
  -f 'names[]=homebrew-tap' \
  -f 'names[]=cli' \
  -f 'names[]=agents' \
  -f 'names[]=github' \
  -f 'names[]=ai-agents' \
  -f 'names[]=open-agent-teams'
```

## After seeding

- Leave `homebrew-oat` **private** until right before cutting the first
  `v*` tag. Unauthenticated `brew tap` won't work against a private
  repo, so flipping it public is the final step of release cutover.
- Provision a `HOMEBREW_TAP_GITHUB_TOKEN` secret on
  `Root-IO-Labs/open-agent-teams` with `contents:write` scoped to
  `Root-IO-Labs/homebrew-oat` only. Then uncomment the `brews:` block
  in `.goreleaser.yml` and the matching env line in
  `.github/workflows/release.yml`.
- Delete `scripts/homebrew-tap-seed/` in the same PR that flips the tap
  public — goreleaser owns the tap contents from that point on.
