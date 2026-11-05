# Change plan: CrossTalk 0.1.4 (Homebrew tap, marketplace ref auto-bump, setup.md gaps, install --check fix, CI hygiene)

Status: draft for the brownfield gate, 2026-11-05, with the operator's interview answers folded in (section 8) and the Codex critique disposition in section 9. No implementation until the operator approves. Evidence tags: VERIFIED-SOURCE (read in this repo at 49efafe, or in `~/Source/homebrew-tap` at 4dd320d), VERIFIED-LIVE (seen on this Mac or GitHub today), VERIFIED-RUN (seen in a 0.1.3 live run report), UNKNOWN (to be settled in the named slice).

Terms: tap = a Git repository of Homebrew formulae, here `GoodOlClint/homebrew-tap`, installed as `goodolclint/tap`. Formula = a Ruby file that tells Homebrew how to install one program; a cask is the other Homebrew kind, for app bundles, and is not used. Keg = the versioned directory Homebrew installs a formula into, `<prefix>/Cellar/crosstalk/<version>`. Prefix = `brew --prefix`: `/opt/homebrew` on Apple silicon, `/usr/local` on Intel macOS, `/home/linuxbrew/.linuxbrew` on Linux. Final tag = a release tag `vX.Y.Z` with no `-rc.N` suffix. Marketplace ref = the `ref` field that pins the plugin source to a release tag in `.claude-plugin/marketplace.json` and `.agents/plugins/marketplace.json`. Ruleset = a GitHub repository rule set on a branch (the newer form of branch protection). Auto-merge = GitHub merges a pull request (PR) by itself once its required checks pass. App token = a short-lived (1 hour) token minted in the workflow from a GitHub App's private key. Slice = one small commit with its own test. DoD = definition of done: what must be true, evidenced, for the slice to count. Fails-before / passes-after = the named test fails on the parent commit and passes on the slice's commit. S / M / L = under 1 hour / about half a day / more than half a day.

## 1. Goal and scope

0.1.4 lets a user install CrossTalk with `brew install goodolclint/tap/crosstalk`, keeps that formula and both marketplace refs current on every final release without hand edits, closes the setup.md gaps the 0.1.3 live runs found, stops `install --check` failing a harness the user removed on purpose, and brings CI to a maintained baseline. Scope: (a) Homebrew tap; (b) marketplace ref auto-bump; (c) setup.md gaps; (d) R4 finding 2; (e) CI hygiene in both repositories, including an audit of the new tap. Out of scope: Windows (own phase), design §11 test 17, the relay gap, orphaned shims (Linux F3), a homebrew-core submission, bottles.

## 2. Existing state (map)

| What | Where | Bearing on 0.1.4 |
|---|---|---|
| Release trigger and gate | `.github/workflows/release.yml:3-6` (`push: tags: ["v*"]`, `workflow_dispatch`), `:8` top-level `permissions: {}`, `:11-14` one job `release` on `macos-latest` with `environment: release` (operator is the required reviewer), `:19-20` job `contents: write` | New jobs go after `release` with `needs: release`, so they run only after the approved job succeeds; each declares its own permissions |
| Tag check | `scripts/release-build.sh:15-34` `check-tag`: tag must be `v$ver` or `v$ver-rc.N`, `$ver` read from `internal/mcpserver/server.go` | An `is-final` decision reuses this grammar: final = exactly `v$ver` |
| rc detection today | `release.yml:121-122`: `case "$TAG" in *-rc.*) pre=(--prerelease)` in the publish step | Inline shell, untested; the new jobs need the same decision as a job output, tested |
| Assets | `release.yml:112-119`: `crosstalk-$TAG-macos-universal.zip`, `crosstalk-$TAG-linux-{amd64,arm64}.tar.gz`, each with `.sha256` (`release-build.sh:66-73`, `:82-98`) | The formula's three `url`/`sha256` pairs come from these six files |
| Signing | `release.yml:42-71` (import, `check-identity`, `codesign --options runtime`), `:74-97` notarize the zip, `:98-106` verify | The Homebrew path must not break the Developer ID signature (section 3.4) |
| Release script tests | `cmd/crosstalk/release_test.go:25-32` `releaseScript` helper runs the script from the repo root; `TestReleaseBuildLinux`, `TestCheckIdentity`, `TestReleaseWorkflowLinuxOutsideSigning` | New script subcommands get tests in the same file with the same helper |
| Binary step | `cmd/crosstalk/install.go:100-102` default bin dir `~/.local/bin`; `:142` `env.Bin = <binDir>/crosstalk`; `:157-158` binary step first; `:319` `installExecutable = os.Executable`; `:324-363` `installBinary` (EvalSymlinks, `os.SameFile` skip, temp file + rename); `:365-382` `installBinaryStep` | Homebrew detection sets `binDir` to `<prefix>/bin` before `:142`; `os.SameFile` already makes the copy a no-op when the target is the Homebrew symlink |
| Registration | Claude `internal/harness/claude/install.go` (`claude mcp add --scope user crosstalk -- <env.Bin> mcp`), Codex `internal/harness/codex/install.go` (`[mcp_servers.crosstalk]` `command = env.Bin`); both read only `env.Bin` | No harness change for Homebrew: registering the Homebrew path is setting `env.Bin` |
| Uninstall binary | `install.go:264-286` `installRemoveSteps`: removes PATH lines for `filepath.Dir(env.Bin)` and `os.Remove(env.Bin)` | Must not delete a Homebrew-owned symlink (section 3.3) |
| Checks K1-K7 | `install.go:454-467` `installChecks` runs every target's `Checks`; targets = every harness whose CLI is on PATH (`:120-133`); Claude K1 K2 K4 K5 K6 (`claude/install.go:537-600`), Codex K1 K2 K3 K5 K6 (`codex/install.go:437-490`), K7 `installPathCheck` (`install.go:418-437`, warning only) | Finding 2: a found harness with nothing installed is still a target, so its K1/K2/K5/K6 fail |
| `Installer` | `internal/harness/harness.go:164-177`: `Found`, `InstallSteps`, `UninstallSteps`, `Checks`, `Registered` (config names `env.Bin`) | Finding 2 needs "any CrossTalk setup at all", which `Registered` does not answer (it is false for an entry naming another path, which K5 must still fail) |
| Marketplaces | `.claude-plugin/marketplace.json` source `ref: "v0.1.2"`, `version: "0.1.3"`; `.agents/plugins/marketplace.json` `ref: "v0.1.2"`. origin/main = 49efafe, PR #8 (refs to v0.1.3) not merged at plan time (VERIFIED-LIVE, `git fetch`) | The bump rewrites both `ref` fields only; `version` already moves with `mcpserver.Version` (`plugin_test.go` `TestManifestsMatchBinary`) |
| Marketplace tests | `cmd/crosstalk/plugin_test.go` `TestManifestsMatchBinary` (versions), `TestMarketplacesPinToReleaseTag` (ref matches `^v[0-9]+\.[0-9]+\.[0-9]+$`) | The bump PR must pass both; a ref equal to an rc tag is already refused |
| CI | `.github/workflows/ci.yml`: `pull_request` and push to main; `permissions: contents: read`; one job `test` on `ubuntu-latest` and `macos-latest`; `actions/checkout@v7`, `actions/setup-go@v7` (no `persist-credentials: false`); build, `go test -race`, vet, gofmt | Pins already above the floor (checkout@v6); no Dependabot, no vulnerability scan, no concurrency group. Check names for a ruleset: `test (ubuntu-latest)`, `test (macos-latest)` |
| Branch protection | CrossTalk main unprotected (handoff OPEN THREADS); tap main unprotected (new repo) | Auto-merge needs a required check on the base branch, so the operator's Q3 answer makes a ruleset a prerequisite on both repos (section 8) |
| Tap repo | `~/Source/homebrew-tap` @ 4dd320d, `brew tap-new` scaffold: `.github/workflows/tests.yml` (`brew test-bot` on push and PR, `macos-26` and `ubuntu-latest` in `ghcr.io/homebrew/brew:main`, `--only-tap-syntax`, `--only-formulae` on PRs, uploads bottles), `publish.yml` (`brew pr-pull`, manual, bottle publish), `autobump.yml` (daily `brew bump --open-pr`), `.github/dependabot.yml` (github-actions only), no `Formula/` | Formula goes in `Formula/crosstalk.rb`; tap audit in slice E6 |
| Current majors | `actions/checkout` latest v7.0.1, `actions/create-github-app-token` latest v3.3.0 (VERIFIED-LIVE, GitHub releases API 2026-11-05) | New workflow lines use these majors; Dependabot keeps them current |

Decision records read: ADR 0013 (plugin and marketplaces), 0015 (signed release, environment gate), 0016 (install registers the absolute path; binary to `~/.local/bin`), 0017 (Linux assets outside signing). Canonical pipelines (AGENTS.md): install touches no message path; Homebrew detection is not harness-specific, so it lives in `cmd/crosstalk/install.go`, not in an adapter.

## 3. (a) Homebrew tap

### 3.1 Formula

`Formula/crosstalk.rb` in the tap, generated per release by `scripts/release-build.sh formula TAG DIST DIST_LINUX` (prints the Ruby file on stdout, reads the three `.sha256` files beside the assets). Shape:

```ruby
class Crosstalk < Formula
  desc "Message other running agent sessions (Claude Code, Codex) by name"
  homepage "https://github.com/GoodOlClint/CrossTalk"
  version "0.1.4"
  license "MIT"
  on_macos do
    url "https://github.com/GoodOlClint/CrossTalk/releases/download/v0.1.4/crosstalk-v0.1.4-macos-universal.zip"
    sha256 "<from .zip.sha256>"
  end
  on_linux do
    on_intel do
      url ".../crosstalk-v0.1.4-linux-amd64.tar.gz"
      sha256 "..."
    end
    on_arm do
      url ".../crosstalk-v0.1.4-linux-arm64.tar.gz"
      sha256 "..."
    end
  end
  def install
    bin.install "crosstalk"
  end
  def caveats
    <<~EOS
      Register CrossTalk with Claude Code and Codex:
        crosstalk install
      Run it again after `brew upgrade crosstalk` so the plugin matches the binary.
      Before `brew uninstall crosstalk`, run:
        crosstalk uninstall
      Restart running Claude and Codex sessions after either.
    EOS
  end
  test do
    assert_equal version.to_s, shell_output("#{bin}/crosstalk version").strip
  end
end
```

A formula (not a cask) because CrossTalk is one command-line binary (gen 4 ruling). No `post_install` that runs `crosstalk install`: it would edit `~/.claude` and `~/.codex` inside Homebrew's install and could not prompt (ADR 0018). No bottles: the formula installs the release binary; `publish.yml` is removed (slice E6).

### 3.2 Release job `homebrew`

Runs on final tags only. The `release` job gains an output `final` set by `scripts/release-build.sh is-final "$TAG"` (prints `true` for exactly `v$ver`, `false` for `v$ver-rc.N`, exits nonzero for anything else). Job `homebrew`: `needs: [release, marketplace]` (it starts only after the marketplace PR has merged, section 4, so a Homebrew user never gets a binary whose plugin ref is not yet published), `if: needs.release.outputs.final == 'true'`, `runs-on: ubuntu-latest`, `environment: homebrew-tap` (holds only the App secrets, no reviewer; the run already passed the `release` approval), job `permissions: {}` (the App token does the writing). Steps: mint an App token with `actions/create-github-app-token@v3` (`owner: GoodOlClint`, `repositories: homebrew-tap`), download the six assets from the published release with `gh release download "$TAG"`, verify each `.sha256`, render the formula, clone the tap with the App token, commit `crosstalk <version>` on branch `crosstalk-<tag>`, push, `gh pr create`, `gh pr merge --auto --squash`. The tap's `tests.yml` runs `brew test-bot` on the PR (PRs opened by an App token trigger workflows, unlike the built-in `GITHUB_TOKEN`); the tap ruleset requires those checks, so the PR merges itself when they pass. An existing branch or open PR for the same tag makes the job fail with its URL, not force-push.

How rc tags are excluded and tested: `TestIsFinal` (script level: `v0.1.4` true, `v0.1.4-rc.1` false, `v0.1.3` nonzero exit when Version is 0.1.4, `v0.1.4-rc.01` nonzero); `TestReleaseWorkflowPostReleaseJobs` (workflow text: `marketplace` has `needs: release`, `homebrew` has `needs: [release, marketplace]`, both have `if: needs.release.outputs.final == 'true'`, the `release` job exports `final` from the `is-final` step, neither job names a signing secret or `environment: release`); live: the next rc tag (`v0.1.4-rc.1`) run shows both jobs skipped.

### 3.3 `crosstalk install` with a Homebrew binary

Detection, in `cmd/crosstalk/install.go`, before `env.Bin` is built: resolve `installExecutable()` with `filepath.EvalSymlinks`; when the result matches `<prefix>/Cellar/crosstalk/<version>/bin/crosstalk` and `<prefix>/bin/crosstalk` resolves to the same file (`os.SameFile`), and `--bin-dir` was not given, use `<prefix>/bin` as the bin dir. The prefix is taken from the path, never hardcoded, so `/opt/homebrew`, `/usr/local` and `/home/linuxbrew/.linuxbrew` all work. Effects:

- Install: the binary step reads "use Homebrew's `<prefix>/bin/crosstalk` (no copy)" and its Apply does nothing (the existing `os.SameFile` skip). Registration, allow rule and checks use `<prefix>/bin/crosstalk`, a path that stays valid across `brew upgrade` (the symlink moves to the new keg). The PATH step is skipped when a login shell already finds `<prefix>/bin` (existing `installOnLoginPath`). When `~/.local/bin/crosstalk` exists from an earlier install, install prints that it is left in place and that `rm ~/.local/bin/crosstalk` removes it (it is not deleted: the user may still run it).
- A Cellar binary whose `<prefix>/bin/crosstalk` is missing or points elsewhere (keg unlinked): install says so and falls back to copying to `~/.local/bin`, as today.
- `--bin-dir DIR` overrides detection: copy to DIR as today.
- Uninstall from a Homebrew binary: the binary step never removes a path under the Homebrew prefix; it prints "Homebrew owns `<prefix>/bin/crosstalk`; run `brew uninstall crosstalk`" (the formula caveats say to run `crosstalk uninstall` first). `--purge` behaves as today.

### 3.4 Signature and quarantine

Homebrew downloads formula sources with its own `curl`, which sets no `com.apple.quarantine` flag; Homebrew applies quarantine only to casks (UNKNOWN for this exact Homebrew version; checked in slice H6). `bin.install` moves the file; Homebrew rewrites Mach-O load commands only for keg-relative dylib links, which a static Go binary has none of, so the Developer ID signature and hardened runtime should survive (UNKNOWN; checked in slice H6 with `codesign --verify --strict` on the keg binary and `xattr -p com.apple.quarantine` expecting no attribute). If either fails, slice H6 stops and the plan returns to the operator: the fallback is a cask, which the gen 4 ruling did not choose.

### 3.5 Slices

| # | Slice | Files | Test: fails before, passes after | DoD |
|---|---|---|---|---|
| H1 | `is-final TAG` subcommand | `scripts/release-build.sh`, `cmd/crosstalk/release_test.go` | `TestIsFinal`: with Version 0.1.4 in a temp copy of `server.go` (or the real Version), `v<ver>` prints `true` exit 0, `v<ver>-rc.1` prints `false` exit 0, `v<ver>-rc.01`, `v<ver>-rc.`, `v0.0.1` exit nonzero. Fails before (unknown subcommand, exit 2) | Green on both CI OSes; `shellcheck` clean |
| H2 | `formula TAG DIST DIST_LINUX` subcommand | `scripts/release-build.sh`, `cmd/crosstalk/release_test.go`, `scripts/crosstalk.rb.tmpl` (or a heredoc in the script) | `TestFormulaRender`: writes three fake assets with real `.sha256` files, renders, asserts `version "<ver>"`, each of three `url` lines names the tag, each `sha256` equals the file's hash, `bin.install "crosstalk"` present; with one `.sha256` missing exit nonzero and nothing on stdout; when `ruby` is on PATH, `ruby -c` accepts the output. Fails before (unknown subcommand) | Green on both CI OSes; rendered file for v0.1.3 passes `brew style` and `brew audit --strict --tap goodolclint/tap crosstalk` on this Mac (output in the PR body) |
| H3 | Release `final` output and job `homebrew` | `.github/workflows/release.yml`, `cmd/crosstalk/release_test.go` | `TestReleaseWorkflowPostReleaseJobs` (section 3.2). Fails before (no such jobs) | `actionlint` clean; operator checklist items 1-4 done before the first final tag; the `v0.1.4-rc.1` run shows `homebrew` skipped; the `v0.1.4` run opens a tap PR that test-bot passes and auto-merge merges |
| H4 | Homebrew detection in install and reinstall | `cmd/crosstalk/install.go`, `cmd/crosstalk/install_test.go` | `TestInstallHomebrewBinary`: temp prefix with `Cellar/crosstalk/0.1.4/bin/crosstalk` and symlink `bin/crosstalk` to it; `installExecutable` returns the symlink; `install --yes` with fake `claude` and `codex` registers `<prefix>/bin/crosstalk` (fake-CLI log and `config.toml`), creates no `~/.local/bin/crosstalk`, K5 PASS. `TestInstallHomebrewUnlinked`: no `bin/crosstalk` symlink: binary copied to `~/.local/bin` and the note printed. `TestInstallHomebrewBinDirOverride`: `--bin-dir` given: copied there. Fails before (registers `~/.local/bin/crosstalk`) | Green on both CI OSes; existing install tests unchanged and green |
| H5a | `--purge` keeps state while any harness still has CrossTalk set up, whatever binary it names | `cmd/crosstalk/install.go` (`installKeep` for the state step uses `SetUp` from D1; the binary step keeps `Registered`, since only a harness running this binary needs this file), `cmd/crosstalk/install_uninstall_test.go` | `TestUninstallPurgeKeepsStateForOtherBinary`: Claude registered at `<prefix>/bin/crosstalk`, Codex at `~/.local/bin/crosstalk`, no running shim; `uninstall claude --purge --yes` run as the Homebrew binary leaves the state dir and prints why. Fails before (state dir deleted, since `Registered` compares to `env.Bin`) | Green on both CI OSes; D1 lands first (Codex finding 1) |
| H5 | Uninstall leaves Homebrew's file | `cmd/crosstalk/install.go`, `cmd/crosstalk/install_uninstall_test.go` | `TestUninstallHomebrewKeepsBinary`: same temp prefix, full `uninstall --yes`: `<prefix>/bin/crosstalk` and the keg file still exist, output names `brew uninstall crosstalk`, MCP entries and allow rule removed. Fails before (symlink deleted) | Green on both CI OSes |
| H6 | First formula in the tap, live check | tap: `Formula/crosstalk.rb` (rendered by H2 for v0.1.3, or for v0.1.4-rc.1 assets), opened as a PR | Tap `tests.yml` passes on the PR (`--only-tap-syntax`, `--only-formulae` on `macos-26` and `ubuntu-latest`). Live on this Mac from the PR branch: `brew install --formula ./Formula/crosstalk.rb` (or `brew tap` the branch), `codesign --verify --strict --verbose=2 "$(brew --prefix)/Cellar/crosstalk/*/bin/crosstalk"` exit 0, `xattr -p com.apple.quarantine` on it reports no such attribute, `crosstalk version` matches | Both checks recorded in the PR; PR closed without merge if it carries v0.1.3 (that binary lacks H4), merged if it carries a 0.1.4 build. If a check fails: stop, report, cask question to the operator |
| H7 | setup.md Homebrew section | `docs/setup.md` | `TestSetupDocCoversGaps` (slice C1) gains `brew install goodolclint/tap/crosstalk` and `brew uninstall crosstalk` | Section says: install line, run `crosstalk install`, re-run after `brew upgrade`, `crosstalk uninstall` before `brew uninstall`, the registered path is `$(brew --prefix)/bin/crosstalk` |

## 4. (b) Marketplace ref auto-bump

Operator answer (Q3, section 8): a PR with auto-merge after CI, on CrossTalk as on the tap. Job `marketplace`: `needs: release`, `if: needs.release.outputs.final == 'true'`, `runs-on: ubuntu-latest`, `environment: homebrew-tap` (same App secrets; renamed `release-bot` if the operator prefers, checklist item 2), job `permissions: {}`. Steps: App token for `CrossTalk` (`repositories: CrossTalk`), checkout `main` with that token, `scripts/release-build.sh bump-marketplaces "$TAG"` (rewrites only the two `ref` values; refuses a non-final tag; refuses a tag older than the ref already on main, compared as versions, so a late older release cannot open a downgrade PR; no-op with exit 0 and no PR when both already equal the tag), `go test ./cmd/crosstalk -run 'TestManifestsMatchBinary|TestMarketplacesPinToReleaseTag'` before pushing, commit `marketplaces: pin plugin to <tag>` on branch `marketplace-<tag>`, push, `gh pr create`, `gh pr merge --auto --squash`, then wait (`gh pr view --json state` every 30 s, 45-minute job timeout) until the PR is `MERGED`, and re-read both refs from `main` to confirm they equal the tag. A PR that does not merge fails the job, so `homebrew` does not run and the formula stays at the previous release.

Why a PR and not a direct commit: the operator chose it; it puts the bump through the same required CI as any change and leaves a reviewable record. Cost: an App installed on CrossTalk (a PR opened by `GITHUB_TOKEN` triggers no workflow, so CI would never report and auto-merge would never fire), and a ruleset on CrossTalk main requiring the CI checks (auto-merge needs a required check, otherwise GitHub refuses `--auto` on a clean PR).

Loop and token risks:

- Loop: the merge commit on main triggers `ci.yml` only. `release.yml` runs on `v*` tags and on `workflow_dispatch`, and its job is guarded by `startsWith(github.ref, 'refs/tags/v')` (`release.yml:12`); nothing in the bump creates a tag. `TestReleaseWorkflowPostReleaseJobs` also asserts `release.yml` has no `push: branches` trigger.
- Wrong ref: `bump-marketplaces` refuses an rc tag; `TestMarketplacesPinToReleaseTag` in CI refuses one too.
- Main moved: the PR is against current main; a conflict leaves the PR open and unmerged, and the job prints its URL (no force-push).
- Token scope: the App has `contents: write` and `pull_requests: write` on the two repos only; its token lives 1 hour and is minted only in the two post-release jobs, which hold no signing secret. Leak impact: a 1-hour write to two repos, gated by the rulesets (no force-push, no deletion, required checks).
- Stale release: `release.yml`'s concurrency group is per tag, so two tags can run at once. `bump-marketplaces` refuses a tag older than main's current ref (Codex finding 3).
- Ordering: the bump job runs after the release is published, so the ref never points at a tag without assets. Between the tag and the merge, users get the previous plugin (today's behaviour).

| # | Slice | Files | Test: fails before, passes after | DoD |
|---|---|---|---|---|
| M1 | `bump-marketplaces TAG` subcommand | `scripts/release-build.sh`, `cmd/crosstalk/release_test.go` | `TestBumpMarketplaces`: on temp copies of both files, `v9.9.9` sets both `ref` to `v9.9.9` and changes no other byte (diff is exactly two lines); `v9.9.9-rc.1` exits nonzero and leaves both files byte-identical; a second run with the same tag exits 0 and prints `unchanged`; with both refs at `v9.9.10`, `v9.9.9` exits nonzero and leaves both files byte-identical (newer release finished first); `v9.10.0` over `v9.9.10` succeeds (version order, not string order). Fails before (unknown subcommand) | Green on both CI OSes; `shellcheck` clean |
| M2 | Job `marketplace` | `.github/workflows/release.yml`, `cmd/crosstalk/release_test.go` | `TestReleaseWorkflowPostReleaseJobs` covers it (section 3.2). Fails before | `actionlint` clean; at `v0.1.4`, a PR `marketplace-v0.1.4` opens, CI runs on it, auto-merge merges it, the job sees `MERGED` and then `homebrew` starts, and no new release run starts (Actions list shows only `ci` for that merge) |

## 5. (c) setup.md gaps

| # | Gap (source) | Change |
|---|---|---|
| 1 | R9-1: `claude` and `codex` not on PATH on a fresh Mac until `eval "$(/opt/homebrew/bin/brew shellenv)"` is in `~/.zprofile` | Step 2: "install finds `claude` and `codex` on the PATH; on a fresh Mac with Homebrew, add Homebrew's `shellenv` line to `~/.zprofile` first" |
| 2 | R9-2: a Codex session's name is its first message text; an extra unnamed idle Codex row may show | Step 4: name a Codex target by its `cwd` when its name is a message; an unnamed idle row is a thread that never ran a turn |
| 3 | R9-3: `crosstalk: false` can be stale in a list taken a minute after the peer's first call | Troubleshooting row: the list is a snapshot; try `ask` anyway, or list again |
| 4 | R9-4: setup.md never says a `crosstalk send` message shows the sender as `@unverified` | New short paragraph on `crosstalk send`: the receiver sees `@unverified`, and a model treats it as an unauthenticated peer |
| 5 | R9-5: line 3 says install has not run on a clean account; after an rc rehearsal the kept marketplace entry points at the scratch path | Line 3 updated (clean-account run 2026-11-05 on v0.1.3-rc.1 passed); step 6 adds `claude plugin marketplace remove crosstalk` / `codex plugin marketplace remove crosstalk` for a full clean-up, and that a rehearsal leaves the entry pointing at the scratch marketplace |
| 6 | R8 F-E: the Linux block has no Claude or Codex install lines | Linux: Claude's `install.sh` puts `claude` in `~/.local/bin`, found after a new login; Codex full package needs its `bin` and `codex-path` directories on PATH in `~/.profile` |
| 7 | R8 F-C and Linux table: Codex's sandbox fails without a working bubblewrap and asks approval to read the plugin's `SKILL.md`; folder-trust dialogs on first run | Linux "Sandbox" bullet: install `bubblewrap`; without it, approve the first skill read; both harnesses show a folder-trust dialog in a new directory |

| # | Slice | Files | Test: fails before, passes after | DoD |
|---|---|---|---|---|
| C1 | All seven gaps plus the Homebrew section from H7 if H7 has landed | `docs/setup.md`, `cmd/crosstalk/docs_test.go` (new) | `TestSetupDocCoversGaps`: setup.md contains `brew shellenv`, `@unverified`, `bubblewrap`, `folder-trust`, `install.sh`, `marketplace remove`, and no longer contains `not yet on a clean account`. Fails before (strings absent) | One line per paragraph or bullet, terms defined on first use; each row of the table above cited by setup.md line in the PR body |

## 6. (d) R4 finding 2: `install --check` after `uninstall codex`

Operator answer (Q4): `--check` with no harness name skips a found harness that has no CrossTalk setup at all, printing `codex: not set up, skipped`; an entry pointing at another binary still fails K5; `--check codex` runs every check. "Set up" = the harness config holds any `crosstalk` server entry (any command) or the CrossTalk plugin is installed. `Registered` cannot answer this (false for an entry naming another path), so `Installer` gains `SetUp(env InstallEnv) (bool, error)`, read from config files only, like `Registered`. An error counts as set up, so the checks run and report it.

| # | Slice | Files | Test: fails before, passes after | DoD |
|---|---|---|---|---|
| D1 | `SetUp` on both adapters and the skip in `installChecks` | `internal/harness/harness.go`, `internal/harness/claude/install.go`, `internal/harness/codex/install.go`, `cmd/crosstalk/install.go`, `cmd/crosstalk/install_test.go` | `TestCheckSkipsHarnessNotSetUp`: fake `claude` and `codex` on PATH, Claude set up, Codex with no table and no plugin: `install --check` exit 0 and prints `codex: not set up, skipped`. Same HOME, `install --check codex`: exit 1. Codex table with `command = "/other/crosstalk"`: `install --check` exit 1 with K5 FAIL. Fails before (exit 1 in case one) | Green on both CI OSes; re-run of the rehearsal's 5f sequence (`install --yes`, `uninstall codex --yes`, `install --check`) in a scratch HOME exits 0 |

## 7. (e) CI hygiene

| # | Slice | Files | Test: fails before, passes after | DoD |
|---|---|---|---|---|
| E1 | Dependabot for `gomod` and `github-actions`, weekly, one grouped PR per ecosystem | `.github/dependabot.yml`, `cmd/crosstalk/ci_test.go` (new) | `TestDependabotEcosystems`: file lists `package-ecosystem: gomod` and `package-ecosystem: github-actions`, each `directory: "/"`. Fails before (no file) | After merge, the repo's Dependabot page lists both ecosystems (operator or coordinator screenshot/URL) |
| E2 | Action pin floor and least privilege, pinned by test | `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `cmd/crosstalk/ci_test.go` | `TestWorkflowHygiene`: every `uses: actions/checkout@vN` has N >= 6, `cache` >= 5, `upload-artifact` >= 5, `download-artifact` >= 6, `create-github-app-token` >= 3; every workflow has a top-level `permissions:`; every job in `release.yml` declares its own `permissions:`; every `actions/checkout` step sets `persist-credentials: false`; any `uses:` outside `actions/` is pinned to a 40-character SHA. Fails before (ci.yml checkout lacks `persist-credentials: false`) | ci.yml adds `persist-credentials: false` and a `concurrency` group that cancels superseded PR runs; `actionlint` clean |
| E3 | Vulnerability scan | `.github/workflows/ci.yml` | `TestWorkflowHygiene` asserts a `govulncheck` job exists, runs `go run golang.org/x/vuln/cmd/govulncheck@vX.Y.Z ./...` (version pinned to the latest `golang/vuln` tag at implementation; the GitHub releases page shows v1.1.4 but tags may be newer), and runs on `pull_request`, push to main and a weekly `schedule`. Fails before | Job green on main; it becomes a required check in the ruleset (operator checklist) |
| E4 | Rulesets (operator action, no code) | none; recorded in ADR 0019 and AGENTS.md | n/a: operator creates them (checklist 5-6) | `gh api repos/GoodOlClint/CrossTalk/rulesets` (coordinator) shows a ruleset on `main` requiring `test (ubuntu-latest)`, `test (macos-latest)`, `govulncheck`, blocking force-push and deletion; tap likewise with test-bot checks |
| E5 | AGENTS.md CI line | `AGENTS.md` | n/a (doc) | Status line lists Dependabot, govulncheck, rulesets; "main unprotected" thread closed |
| E6 | Tap audit | tap: `.github/workflows/publish.yml` (delete), `.github/workflows/autobump.yml` (delete), `.github/workflows/tests.yml` (keep; add `concurrency`), `.github/dependabot.yml` (keep), `README.md` (crosstalk line) | Tap PR: `tests.yml` green; `actionlint` clean | Reason in the PR body: no bottles (publish is unused), the release job is the formula's one writer (autobump would race it and needs a `livecheck`). Remaining tap actions are Homebrew's own, SHA-pinned (VERIFIED-SOURCE); top-level `permissions: {}` already |

## 8. Operator interview (2026-11-05)

Asked in this terminal, one batch of four plus one follow-up. Settled by earlier rulings and not asked: formula not cask, tap repo `GoodOlClint/homebrew-tap`, release.yml job on final tags pushing the formula, install registers the stable Homebrew bin path, caveats, ADR and setup.md (gen 4 ruling); R8/R9 gaps go into setup.md (gen 5 R8, R9); Windows is its own phase.

| Q | Question | Answer | Where it lands |
|---|---|---|---|
| Q1 | Credential for the tap push | GitHub App (recommended), minted per run with `actions/create-github-app-token` | Section 3.2; checklist 1-3; ADR 0018 |
| Q2 | Where the tap push runs relative to the `release` gate | Own job, own environment holding only the App secrets, no reviewer; `needs: release` (recommended) | Section 3.2; ADR 0018 |
| Q3 | How the marketplace refs move | First answer: "homebrew-tap is brand new and should likely be audited as well. I am thinking we do a pr with auto-merge after tests for our release pipeline?" Then: "why do we need an app on the crosstalk repo? My thought was the tap should auto-merge PRs if they come from a release workflow". Follow-up asked; final answer: PR + auto-merge on CrossTalk too (against the recommendation of a direct `GITHUB_TOKEN` commit) | Section 4; tap audit is slice E6; App on both repos; rulesets on both mains become prerequisites (the kickoff listed branch protection as optional; this answer makes it required); ADR 0019 |
| Q4 | Finding 2 fix shape | Skip a found harness with no CrossTalk setup when no harness is named; K5 still fails a wrong entry (recommended) | Section 6 |

## 9. Operator creates (checklist)

1. A GitHub App, e.g. `crosstalk-release`, owned by GoodOlClint: no webhook; repository permissions Contents read/write, Pull requests read/write, Metadata read; installed on `GoodOlClint/homebrew-tap` and `GoodOlClint/CrossTalk` only.
2. In CrossTalk, environment `homebrew-tap` (or `release-bot`; tell the implementer which), deployment branches and tags limited to `v*` tags, no required reviewer.
3. Environment secrets `RELEASE_APP_ID` (the App's ID) and `RELEASE_APP_PRIVATE_KEY` (a `.pem` from the App's settings). Not repository secrets, so a PR workflow cannot read them.
4. Settings, General, "Allow auto-merge" on in both repos.
5. CrossTalk ruleset on `main`: require status checks `test (ubuntu-latest)`, `test (macos-latest)`, `govulncheck` (after E3 lands); block force-push and deletion; bypass list: repository admin (the operator), so a hand merge still works. Require a pull request only if you want your own pushes to go through PRs too.
6. Tap ruleset on `main`: require `test-bot (macos-26)` and `test-bot (ubuntu-latest)`; block force-push and deletion; bypass: repository admin.
7. Signing cert renewal before 2027-02-01 (existing thread; not 0.1.4 work).

## 10. Sequencing and release

Order: D1 (then H5a, which uses its `SetUp`), C1 and E1-E3 first (independent, no secrets). Then H1, H2, M1 (script subcommands). Then H4, H5, H5a (install). Then operator checklist 1-6 and E6 (tap audit). Then H3 and M2 (workflow jobs), H6 (first formula live), H7 into setup.md. Version bump to 0.1.4 rides with the last code slice. Tag `v0.1.4-rc.1`: both post-release jobs must show skipped. Tag `v0.1.4`: tap PR and marketplace PR both open and auto-merge; `brew install goodolclint/tap/crosstalk` on this Mac then `crosstalk install --check` exit 0 with K5 naming `$(brew --prefix)/bin/crosstalk`. Size: H1-H5 M, H6 S plus live time, M1-M2 S, C1 S, D1 S, E1-E6 M. About two days of slices plus operator setup.

## 11. Review disposition

Codex adversarial review, `node codex-companion.mjs adversarial-review --wait --base main` on 68e1d1c (`/tmp/crosstalk-0.1.4-codex.log`): verdict needs-attention, 3 findings, all checked against the code and all fixed in the plan (commit in the report; this section lands in the same commit).

| # | Severity | Finding | Checked | Disposition |
|---|---|---|---|---|
| 1 | high | `--purge` from a Homebrew binary can delete shared state while the other harness still runs a `~/.local/bin` copy: `installKeep` uses `Registered`, which compares to `env.Bin` | True: `install.go:251-262` and `:288-301`; `codex/install.go:425` `e.command == env.Bin` | Fixed: slice H5a, state guard uses `SetUp` (any setup, any path); test for a mixed install |
| 2 | medium | The tap PR can merge while the marketplace PR is still pending or failed, so a Homebrew user's `crosstalk install` gets the old plugin and K1 fails | True: both jobs had only `needs: release`; auto-merge is asynchronous | Fixed: `marketplace` waits until its PR is `MERGED` and refs on main equal the tag, else fails; `homebrew` needs `marketplace`. The same window still exists for curl-download users between publish and merge (as today), named not closed |
| 3 | medium | An older release finishing after a newer one opens a downgrade PR that passes the ref-syntax test | True: concurrency group is per tag (`release.yml:15-17`); `TestMarketplacesPinToReleaseTag` checks syntax only | Fixed: `bump-marketplaces` refuses a tag older than main's ref (version compare); `TestBumpMarketplaces` covers newer-first and `v9.10.0` over `v9.9.10` |
