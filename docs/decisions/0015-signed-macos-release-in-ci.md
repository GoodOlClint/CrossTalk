# ADR 0015 — A version tag builds, signs, notarizes and publishes a universal macOS binary in GitHub Actions

- **Status:** Provisional (operator 2026-11-05)
- **Date:** 2026-11-05
- **Deciders:** operator + Claude (Sonnet 5.5)
- **Context source:** `docs/setup.md` step 1, ADR 0013 (plugins), the Athena release workflow (pattern only)

## Context

A stranger installs CrossTalk with `go install`, which needs Go and a build. The repo is public, so a signed binary on the GitHub release removes that step. The plugins call `crosstalk` by name, so the binary on the PATH is the one piece the plugin cannot ship. Athena has a release workflow with the same signing steps, but it is marked authored-but-inert: its signing and notarize steps skip when secrets are absent, so that pattern may never have run signed.

## Decision

Pushing a `v*` tag runs `.github/workflows/release.yml` on `macos-latest`. It checks that the tag equals `v` plus `mcpserver.Version`, builds arm64 and amd64 separately (`CGO_ENABLED=0`, `-trimpath`), joins them with `lipo` into one universal binary, signs it with a Developer ID Application certificate (`codesign --timestamp --options runtime`), zips it with `ditto`, notarizes the zip with `notarytool submit --wait` using an App Store Connect API key, verifies the signature, and publishes the zip and a SHA-256 file to the release for that tag with the runner's preinstalled `gh` and the job's `GITHUB_TOKEN` (no third-party action, so there is nothing to pin by sha). A `workflow_dispatch` trigger with a required `tag` input retries a failed run without moving the tag. The build, tag check, packaging and checksum steps live in `scripts/release-build.sh`, so the local proof runs the same code the workflow runs.

The operator accepts that the Developer ID signature carries their legal name, visible on the public binary.

A bare Mach-O binary cannot be stapled (the notarization ticket attached to the file). macOS finds the ticket online on first run. `docs/setup.md` says what is and is not verified about the first-run behaviour.

Public-repo secret handling:

- No `pull_request` trigger. Only a tag push by someone with write access, or a manual dispatch, starts the job.
- The job is attached to the environment `release`. The six secrets are environment secrets there, not repo secrets, so only a job that names the environment can read them.
- Top-level `permissions: {}`; the job alone gets `contents: write`.
- Each secret is in the `env` of the one step that needs it, never job-level. Key material is written under `RUNNER_TEMP` with `umask 077` and removed; the temporary keychain is deleted in a step with `if: always()`.
- First-party actions are at the current major (`actions/checkout@v7`, `actions/setup-go@v7`, as `ci.yml`). Any third-party action added later is pinned to a full commit sha.
- The operator sets a tag-protection ruleset for `v*` limited to maintainers, and limits the environment's deployment branches and tags to `v*`.

## Rejected alternatives

- **Sign locally and upload by hand.** Needs the operator's keychain and attention on every release, and the result is not reproducible from the tag.
- **Athena's workflow as is.** Job-wide secret env, no environment, unpinned actions, steps that silently skip, and app-specific parts (Xcode, Rust, .pkg, Installer identity).
- **Skip signing.** A downloaded unsigned binary runs into Gatekeeper and gives a stranger a worse install than `go install`.
- **A third-party release action.** One more sha to pin and trust, for what `gh release create` does.
- **A Homebrew tap.** Deferred.

## Consequences

- The first run is unproven. Nothing here has run with real credentials: the local proof covers the build, `lipo`, version check, zip, checksum and an ad hoc signature, not the certificate import, the notarization status or `spctl`. Watch on the first run: the certificate import step, the `notarytool` status (must be `Accepted`), the `spctl` line (a bare binary may print "rejected"; the check that counts is `codesign --verify --strict`), `lipo -archs` (both `arm64` and `x86_64`), and the release assets `crosstalk-<tag>-macos-universal.zip` and `.zip.sha256`.
- A release needs the Mac-only toolchain on the runner (`codesign`, `notarytool`, `lipo`); `macos-latest` provides it.
- A failed run leaves no release if it fails before publish; rerun with `workflow_dispatch` and the same tag.
- Linux binaries are not built.

## Operator setup

Nothing here is set by the repo. Create these as environment secrets in the `release` environment (Settings, Environments):

- `APPLE_CERT_P12`: the Developer ID Application certificate and its private key exported from Keychain Access as a `.p12`, then `base64 -i cert.p12`.
- `APPLE_CERT_PASSWORD`: the password chosen at that export.
- `APPLE_CODESIGN_IDENTITY`: the identity string from `security find-identity -v -p codesigning`, for example `Developer ID Application: Name (TEAMID)`.
- `AC_API_KEY_P8`: the App Store Connect API key file (`.p8`), `base64 -i AuthKey_XXXX.p8`.
- `AC_API_KEY_ID`: that key's id.
- `AC_API_ISSUER`: the issuer id shown on the App Store Connect keys page.

Environment rules: deployment branches and tags limited to `v*`; optionally a required reviewer. Add a tag protection ruleset for `v*` limited to maintainers. Actions permissions: the repo's workflow token default can stay read-only, since the job asks for `contents: write` itself.

## Trial record

- Rests on: ADR 0013 (the binary the plugins start).
- Local proof (2026-11-05): `scripts/release-build.sh` build, package and tag checks run with signing skipped; ad hoc `codesign` and `--verify --strict` on the universal binary; `actionlint` clean.
