# ADR 0019 — Marketplace refs move to a final tag by an auto-merged pull request

- **Status:** Provisional (operator 2026-11-05)
- **Date:** 2026-11-05
- **Deciders:** operator (ask 2026-11-05 to auto-bump; interview 2026-11-05 Q3) + Claude (Opus 5.5)
- **Context source:** `docs/plans/release-0.1.4.md` sections 4, 7 and 8; ADR 0013, 0015

## Context

Both marketplace files pin the plugin source to a release tag (`ref`, plan 0.1.3 Q11), so `claude plugin install` and `codex plugin add` fetch the plugin at that tag. The ref can only move after the tag exists, so every release has needed a hand-made follow-up commit (0.1.3: PR #8), and until it lands users of the published marketplace get the previous plugin while the Claude file's `version` already names the new one. CrossTalk's `main` had no branch protection. GitHub auto-merge needs at least one required check on the base branch, and a PR opened with the built-in `GITHUB_TOKEN` triggers no workflow, so its checks would never report.

## Decision

- On a final tag only, a `marketplace` job in `release.yml` runs after the `release` job publishes: it mints a 1-hour token from the same GitHub App as ADR 0018 (installed on CrossTalk with Contents and Pull requests write; client id and key from the shared `homebrew-tap` environment), rewrites only the two `ref` values with `scripts/release-build.sh bump-marketplaces TAG`, runs the marketplace tests, pushes branch `marketplace-<tag>`, and hands it to `scripts/release-build.sh land-pr`, which opens or reuses the PR, enables auto-merge and waits until it is merged; the job then confirms `main`'s refs equal the tag, and fails otherwise. Re-running a failed job is safe: an existing branch or PR is reused, a ref already on `main` is a no-op. `bump-marketplaces` refuses a tag older than `main`'s current ref, so a late older release cannot downgrade it.
- When the repo refuses auto-merge, `land-pr` merges the PR itself once GitHub reports it `CLEAN` (required checks passed) and fails at once on a failed required check (3622409, 148607a). That fallback is only as strong as the required checks the target repo's ruleset sets.
- The `homebrew` job (ADR 0018) needs this job, so the formula moves only after the plugin ref it depends on is on `main`.
- A ruleset on CrossTalk's `main` requires the CI checks (`test (ubuntu-latest)`, `test (macos-latest)`; `govulncheck` runs but is not required, so a new advisory in a dependency cannot block the bump PR) and blocks force-push and deletion, with the operator as bypass. The tap gets the same with its test-bot checks.
- The bump never creates a tag, and `release.yml` triggers only on `v*` tags (and manual dispatch guarded to tags), so the bump's merge runs `ci.yml` alone.

## Rejected alternatives

- **Direct commit to `main` with `GITHUB_TOKEN`.** No App on CrossTalk and no ruleset needed, no loop (its pushes trigger nothing), but the commit skips CI on `main`; recommended in the interview, the operator chose the PR so every change to `main` goes through the same checks.
- **Direct commit with the App token.** CI runs after the fact, not before; nothing gates the commit.
- **PR without auto-merge.** Refs lag until someone merges, which is the gap the ask closes.
- **Setting the ref to the coming tag in the version-bump PR.** `main`'s marketplace would name a tag that does not exist yet, so installs fail between merge and tag.

## Consequences

- Branch protection on `main`, an open thread the kickoff called optional, becomes a prerequisite for auto-merge on both repos (operator creates it; plan section 9).
- The operator's own direct pushes to `main` keep working through the ruleset bypass.
- A bump PR that conflicts or fails CI stays open; the job prints its URL and the release itself is already published. Recovery steps (plan section 4.1) go into ADR 0015's operator list.
- A known-vulnerable dependency does not block `main`; the weekly scheduled `govulncheck` run is how it surfaces.
- `TestMarketplacesPinToReleaseTag` already refuses an rc ref, so a wrong bump fails CI rather than merging.

## Trial record

- Rests on: ADR 0018 (shared App), ADR 0015 (release job and its gate).
- Commits (no `Provisional-ADR` trailer was used): bc33868 and 508c253 M1 `bump-marketplaces`; 0a748fc `land-pr`; 03f0a12 M2 `marketplace` job; 3622409 and 148607a `land-pr` direct-merge fallback.
- Pinning tests: `TestBumpMarketplaces`, `TestLandPR`, `TestReleaseWorkflowPostReleaseJobs`.
- Run live: bump PRs #21 (`v0.1.4`, 8161db3) and #27 (`v0.1.5`, 49121ea) merged by `crosstalk-release[bot]`; the releases list shows no release from either merge. The ruleset on `main` now exists (AGENTS.md, operator 2026-11-05).

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (Trial record was "none yet"; `land-pr` direct-merge fallback; shared environment). Kept separate from ADR 0018. Status stays Provisional.
