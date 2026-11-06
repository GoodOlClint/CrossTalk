# ADR 0015 — A version tag builds, signs, notarizes and publishes a universal macOS binary in GitHub Actions

- **Status:** Provisional (operator 2026-11-05)
- **Date:** 2026-11-05
- **Deciders:** operator + Claude (Sonnet 5.5)
- **Context source:** `docs/setup.md` step 1, ADR 0013 (plugins), the Athena release workflow (pattern only)

## Context

A stranger installs CrossTalk with `go install`, which needs Go and a build. The repo is public, so a signed binary on the GitHub release removes that step. The plugins call `crosstalk` by name, so the binary on the PATH is the one piece the plugin cannot ship. Athena has a release workflow with the same signing steps, but it is marked authored-but-inert: its signing and notarize steps skip when secrets are absent, so that pattern may never have run signed.

## Decision

Pushing a `v*` tag runs `.github/workflows/release.yml` on `macos-latest`. The job runs only when the ref is a tag starting `v` (`if: startsWith(github.ref, 'refs/tags/v')`), and the tag is always `github.ref_name`: there is no tag input, so what is built is the tagged tree and nothing else. It checks that the tag equals `v` plus `mcpserver.Version`, or that plus `-rc.N` (a release candidate for rehearsing with real secrets; it publishes as a GitHub prerelease, and any other suffix is refused), runs `go test ./...`, builds arm64 and amd64 separately (`CGO_ENABLED=0`, `-trimpath`), joins them with `lipo` into one universal binary, signs it with a Developer ID Application certificate (`codesign --timestamp --options runtime`) and deletes the temporary keychain at once, zips it with `ditto` (the zip holds exactly one entry, `crosstalk`, checked after zipping), notarizes the zip with `notarytool submit --wait` using an App Store Connect API key (the key file is removed when the step exits), verifies the signature and the notarization (`codesign --verify --check-notarization -R=notarized`), and publishes the zip and a SHA-256 file with the runner's preinstalled `gh` and the job's `GITHUB_TOKEN` (no third-party action, so nothing to pin by sha). Publishing goes through a draft release that is then made public; a retry may replace assets only on a draft, never on a published release. The checkout uses `refs/tags/<tag>`, runs for one tag are serialized, and `setup-go` runs with `cache: false`, because `ci.yml` on main writes the same cache key and a signed build must not restore objects built by code on main. A `workflow_dispatch` trigger exists, but the job only runs when it is dispatched from a tag ref, so a retry is `gh workflow run release.yml --ref <tag>` or "Re-run failed jobs", never from `main`. The build, tag check, packaging and checksum steps live in `scripts/release-build.sh`, so the local proof runs the same code the workflow runs.

The operator accepts that the Developer ID signature carries their legal name, visible on the public binary.

A bare Mach-O binary cannot be stapled (the notarization ticket attached to the file). macOS finds the ticket online on first run. `docs/setup.md` says what is and is not verified about the first-run behaviour.

Public-repo secret handling:

- No `pull_request` trigger. Only a tag push by someone with write access, or a dispatch from a tag, starts the job.
- The job is attached to the environment `release`, and the six secrets are environment secrets there, not repo secrets. The environment's deployment rule is "Selected branches and tags" with one rule of ref type Tag and pattern `v*` and no Branch rule, set before any secret is added (a new environment has no restriction). The operator is the required reviewer, so signing never runs unapproved.
- Top-level `permissions: {}`; the job alone gets `contents: write`.
- Each secret is in the `env` of the one step that needs it, never job-level. Key material is written under `RUNNER_TEMP` with `umask 077` and removed; the temporary keychain is deleted right after signing and again in a step with `if: always()`.
- First-party actions are at the current major (`actions/checkout@v7`, `actions/setup-go@v7`, as `ci.yml`). Any third-party action added later is pinned to a full commit sha.
- A tag ruleset on `refs/tags/v*`, Active, restricts creation, update and deletion and blocks force pushes, with bypass for Repository admin only.

## Rejected alternatives

- **Sign locally and upload by hand.** Needs the operator's keychain and attention on every release, and the result is not reproducible from the tag.
- **Athena's workflow as is.** Job-wide secret env, no environment, unpinned actions, steps that silently skip, and app-specific parts (Xcode, Rust, .pkg, Installer identity).
- **Skip signing.** A downloaded unsigned binary runs into Gatekeeper and gives a stranger a worse install than `go install`.
- **A third-party release action.** One more sha to pin and trust, for what `gh release create` does.
- **A Homebrew tap.** Deferred.

## Consequences

- The first run is unproven. Nothing here has run with real credentials: the local proof covers the build, `lipo`, version check, zip, checksum and an ad hoc signature, not the certificate import, the notarization status or `spctl`. Watch on the first run: the certificate import step, the `notarytool` status (must be `Accepted`), the `spctl` line (a bare binary may print "rejected"; the check that counts is `codesign --verify --check-notarization -R=notarized`), `lipo -archs` (both `arm64` and `x86_64`), and the release assets `crosstalk-<tag>-macos-universal.zip` and `.zip.sha256`.
- A release needs the Mac-only toolchain on the runner (`codesign`, `notarytool`, `lipo`); `macos-latest` provides it.
- A run that fails before publish leaves no release, or a draft; rerun from the tag as above. A published release is never overwritten, and once immutable releases are on, deleting it and rerunning under the same tag fails: a bad published release needs a new tag. A tag is public to the Go module proxy at once, so a fix to `release.yml` needs a new tag (a new `-rc.N`, or a bumped version), not a moved one. Rehearse with `v0.1.2-rc.1` before the real `v0.1.2`.
- A rc run rehearses everything except the plain-tag path through the publish step; that path is first exercised by `v0.1.2` itself.
- The x86_64 slice is built and inspected but has never been executed (the runner is arm64).
- Linux binaries are built and packaged in the same job, outside the signing path, as built by [ADR 0017](0017-linux-release-assets-outside-the-signing-path.md).

## Operator setup

Nothing here is set by the repo. In order:

1. Create the environment `release` (Settings, Environments). Before adding any secret, set Deployment branches and tags to "Selected branches and tags" with one rule of ref type Tag, pattern `v*`, and no Branch rule. Add a required reviewer (yourself) and leave "Prevent self-review" off: you push the tag and are the only reviewer, so with it on nobody can approve.
2. Add these environment secrets in `release`, not repo secrets. Pipe base64 output to `pbcopy` rather than the terminal.
   - `APPLE_CERT_P12`: the Developer ID Application certificate and its private key exported from Keychain Access as a `.p12`, then `base64 -i cert.p12 | pbcopy`.
   - `APPLE_CERT_PASSWORD`: the password chosen at that export.
   - `APPLE_CODESIGN_IDENTITY`: the identity string from `security find-identity -v -p codesigning`, for example `Developer ID Application: Name (TEAMID)`.
   - `AC_API_KEY_P8`: the App Store Connect API key file (`.p8`), `base64 -i AuthKey_XXXX.p8 | pbcopy`. Create it as a Team key with the Developer role (unverified: check the role Apple requires).
   - `AC_API_KEY_ID`: that key's id.
   - `AC_API_ISSUER`: the issuer id shown on the App Store Connect keys page.
3. Delete the local `.p12` and `.p8` copies after upload.
4. Add a tag ruleset: target `refs/tags/v*`, Active, restrict creations, updates and deletions, block force pushes, bypass Repository admin only, no App. Afterwards check that the ruleset's `conditions.ref_name.include` is exactly `refs/tags/v*` (`GET /repos/GoodOlClint/CrossTalk/rulesets/<id>`); GitHub's docs do not say whether the field takes `v*` or the full ref.
5. Actions permissions: the repo's workflow token default can stay read-only, since the job asks for `contents: write` itself.
6. Recommended: enable immutable releases, so published assets and tags are locked (draft then publish is the pattern GitHub recommends for it). Recommended: protect `main`, or read `git diff <last-tag>..<new-tag> -- .github scripts` before approving a run.
7. Rehearse: push `v0.1.2-rc.1` (it publishes a prerelease), watch the run, then tag the real `v0.1.2` only after an rc run succeeds.
8. Recovering a half-published release (the `release` job published the assets, then `marketplace` or `homebrew` failed; plan section 4.1). Never delete, re-tag or re-upload a published release. In order:
   1. Read the failed job's last lines: `land-pr` prints the PR URL and its `mergeStateStatus`.
   2. Fix the cause: a failed CI or test-bot check (push a fix to the PR branch, or to `main` and update the branch), a merge conflict (rebase the PR branch on `main`), or a missing secret or App installation.
   3. Actions, the release run, "Re-run failed jobs". `land-pr` reuses the open PR or the existing branch and waits again; a PR merged meanwhile exits 0. `homebrew` runs once `marketplace` succeeds.
   4. If a re-run cannot work (the App is gone, or the run is past GitHub's re-run window): from a clean checkout of `main`, `scripts/release-build.sh bump-marketplaces vX.Y.Z`, PR, merge on green CI; then in a tap checkout, `gh release download vX.Y.Z` into an empty directory, `scripts/release-build.sh formula vX.Y.Z DIR DIR > Formula/crosstalk.rb`, PR, merge on green test-bot. Marketplace first, tap second.
   5. Until it is fixed, a curl-download user gets the previous plugin (K1 fails until the ref moves) and a Homebrew user the previous formula; existing installs are unaffected.

## Trial record

- Rests on: ADR 0013 (the binary the plugins start).
- Local proof (2026-11-05): `scripts/release-build.sh` build, package, zip listing, checksum and tag checks run with signing skipped; ad hoc `codesign` and `--verify --strict` on the universal binary; the `setup.md` download commands run against a local zip in a scratch HOME; `actionlint` and `shellcheck` clean.
- Verifier and Codex reviews of the first version drove the dispatch, cache, zip-layout and operator-list changes above.
