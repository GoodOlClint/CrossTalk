# ADR 0017 — Linux release assets are cross-built in the release job and kept out of the signing path

- **Status:** Provisional (operator 2026-11-05)
- **Date:** 2026-11-05
- **Deciders:** operator (ruling 2026-11-05: Linux in 0.1.3) + Claude (Opus 5.5)
- **Context source:** `docs/plans/release-0.1.3.md` section (b); a Linux test VM run (all seven tests pass; setup.md step 1 had no Linux asset, so the binary was cross-built by hand); ADR 0015

## Context

The code builds and tests on Linux (CI runs `ubuntu-latest`), and a Linux test VM run passed, but a Linux user has no release asset and no Go on the target machine. The release job (ADR 0015) runs on `macos-latest` and signs and notarizes the one macOS binary. Linux binaries need no signature, and a Linux file must never reach `codesign` or `notarytool`.

## Decision

- The release job builds `linux/amd64` and `linux/arm64` (`CGO_ENABLED=0`, `-trimpath`) with a new `scripts/release-build.sh build-linux OUTDIR` into its own directory, `$RUNNER_TEMP/dist-linux`. The signing, notarize and verify steps name only `$RUNNER_TEMP/dist`, so no Linux file reaches them.
- Each Linux binary is packaged as `crosstalk-<tag>-linux-<arch>.tar.gz` holding exactly one entry, `crosstalk` (tar keeps the execute bit), with a `.tar.gz.sha256` beside it, by `scripts/release-build.sh package-linux OUTDIR TAG`.
- The Linux build and package steps run after the main CI gate (the job checks the `ci.yml` run on `main` for the tagged commit and runs no tests itself; ADR 0015) and before the certificate import, and the publish step uploads the Linux files in the same draft release as the macOS zip.
- arm64 is built because it costs one more `go build`; it is not executed anywhere before release.

## Rejected alternatives

- **A separate `ubuntu-latest` job.** Needs artifact passing between jobs and a second job attached to the `release` environment; one job keeps one draft-then-publish step.
- **Bare binaries.** A browser download loses the execute bit.
- **A combined `SHA256SUMS` file.** The macOS asset uses one `.sha256` per file; setup.md checks each the same way.
- **Signing Linux binaries (cosign, GPG).** No user asked; a checksum from the same release matches the macOS checksum story.

## Consequences

- ADR 0015's "Linux binaries are not built" consequence is replaced by a pointer here (edited in place).
- `linux/arm64` ships unexecuted; setup.md says so.
- The release job gets two cross-builds longer (seconds).
- A Linux asset is only as trustworthy as the GitHub release and the `.sha256` beside it; there is no signature.

## Trial record

- Rests on: ADR 0015.
- Commits: 6a90282 (build-linux and package-linux), b25d37c (release job builds, packages and publishes the Linux assets). Implementing commits carry `Provisional-ADR: 0017`.
- Pinning test: `TestReleaseWorkflowLinuxOutsideSigning` (no step that runs `codesign`, `notarytool`, `security` or `lipo` names `dist-linux`; the publish step lists the four Linux files).
- Run live: the `homebrew` job for `v0.1.4` and `v0.1.5` downloaded the release and passed only with exactly three verified `.sha256` files (`.github/workflows/release.yml`, "Download and verify assets"), the macOS zip and both Linux tarballs. Not yet run live: a release tarball unpacked and executed on a Linux host.
- Pinning test: `TestReleaseBuildLinux` (plan section (b), slice B1).

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (the job runs no `go test`; it checks main CI). The chair proposed merging this ADR into ADR 0015; both advisors kept it separate because unsigned Linux assets are their own trust decision, and the chair agreed. Status stays Provisional.
