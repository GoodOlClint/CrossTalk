# ADR 0018 — Homebrew tap formula and crosstalk install registers the Homebrew path

- **Status:** Provisional (operator 2026-11-05)
- **Date:** 2026-11-05
- **Deciders:** operator (gen 4 ruling on the tap shape; interview 2026-11-05 Q1, Q2) + Claude (Opus 5.5)
- **Context source:** `docs/plans/release-0.1.4.md` sections 3 and 8; ADR 0015, 0016, 0017

## Context

Today a user downloads a release asset, checks its `.sha256` and runs `./crosstalk install`, which copies the binary to `~/.local/bin` and registers that absolute path with Claude Code and Codex (ADR 0016). macOS users expect `brew install`. The operator created `GoodOlClint/homebrew-tap` (a `brew tap-new` scaffold with no formula). The release job (ADR 0015) signs and notarizes the macOS universal binary and publishes six assets (ADR 0017); the formula must use those exact files, not a second build. A Homebrew binary lives in a versioned keg (`<prefix>/Cellar/crosstalk/<version>/bin/crosstalk`) that `brew upgrade` replaces, so registering the resolved path would break on every upgrade.

## Decision

- Distribute through a formula, `Formula/crosstalk.rb` in `GoodOlClint/homebrew-tap`, that downloads the release assets (`macos-universal.zip` on macOS, `linux-amd64` or `linux-arm64` `.tar.gz` on Linux) with their published SHA-256 sums and runs `bin.install "crosstalk"`. No bottles, no `post_install`.
- `scripts/release-build.sh formula` renders the formula from the assets' `.sha256` files. A `homebrew` job in `release.yml`, after the `release` job and after the `marketplace` job has merged the new plugin ref (ADR 0019), only on a final tag (`is-final`), mints a 1-hour GitHub App token for the tap, opens a PR with the new formula and enables auto-merge; the tap's `brew test-bot` checks gate the merge.
- The job runs in its own environment that holds only the App's secrets, with no required reviewer: it runs only after the operator approved the `release` job, and never sees the signing secrets.
- `crosstalk install` detects a Homebrew keg binary (resolved path under `<prefix>/Cellar/crosstalk/`, with `<prefix>/bin/crosstalk` resolving to the same file) and uses `<prefix>/bin/crosstalk` as the installed binary: no copy, and that path is what both harnesses register. `--bin-dir` overrides detection; an unlinked keg falls back to the copy.
- `uninstall --purge` keeps the state directory while any harness still has CrossTalk set up, whichever binary path it names, because a Homebrew and a `~/.local/bin` install can coexist.
- `crosstalk uninstall` never deletes a file under the Homebrew prefix; it points to `brew uninstall crosstalk`. The formula's caveats say to run `crosstalk install` after install and after `brew upgrade`, and `crosstalk uninstall` before `brew uninstall`.

## Rejected alternatives

- **A cask.** For app bundles; CrossTalk is one command-line binary (gen 4 ruling). Stays the fallback if Homebrew breaks the signature or quarantines the binary (plan slice H6).
- **Building from source in the formula (`depends_on "go"`).** A second, unsigned build that differs from the released, notarized binary.
- **`post_install` running `crosstalk install`.** Edits `~/.claude` and `~/.codex` from inside Homebrew without a terminal to confirm on; ADR 0016 requires a confirmed step per change.
- **Registering the keg path or `<prefix>/opt/crosstalk/bin/crosstalk`.** The keg path changes on every upgrade. The `opt` path is also stable, but `<prefix>/bin` is what the user's shell already finds, so K5 (registered command) and K7 (login shell) name one path.
- **A fine-grained personal access token.** Expires, acts as the operator, and is broader in time than a per-run App token (interview Q1).
- **Pushing the formula straight to the tap's main.** The operator chose PRs gated by the tap's own tests (interview Q3).

## Consequences

- The operator creates a GitHub App installed on the tap (and CrossTalk, ADR 0019), an environment with its two secrets, auto-merge on, and a ruleset on the tap's main requiring the test-bot checks (plan section 9).
- Every final release opens one tap PR; an rc tag opens none.
- After `brew upgrade crosstalk`, the registered path is unchanged, but the plugin stays at the old version until the user runs `crosstalk install` again (caveats say so; K1 reports it).
- The tap's generated `publish.yml` and `autobump.yml` are removed (plan slice E6): the release job is the formula's one writer.

## Trial record

- Rests on: ADR 0015, 0016, 0017; ADR 0019 (shared App and auto-merge pattern).
- Commits: none yet. Implementing commits carry `Provisional-ADR: 0018`.
- Pinning tests (planned): `TestIsFinal`, `TestFormulaRender`, `TestReleaseWorkflowPostReleaseJobs`, `TestInstallHomebrewBinary`, `TestInstallHomebrewUnlinked`, `TestInstallHomebrewBinDirOverride`, `TestUninstallHomebrewKeepsBinary`, `TestUninstallPurgeKeepsStateForOtherBinary`.
- Not yet run live: signature and quarantine of the keg binary on this Mac (slice H6); the first final-tag tap PR.
