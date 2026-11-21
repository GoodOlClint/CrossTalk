# ADR 0029 — Harness releases are re-validated by a scheduled CI job, and the queue gate trusts the schema, not a version list

- **Status:** Provisional (operator 2026-11-20; amended 2026-11-21 for the leg matrix, ruling R229, and again 2026-11-21 for the job renames, the four legs and the single usage read; record, detect, tier 0 and the Linux round trip are built, the restructure and the rest are not)
- **Date:** 2026-11-20
- **Deciders:** operator (rulings 2026-11-20, amendment on macOS runners the same day) + Claude (Opus 5.5)
- **Context source:** [change plan](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/plans/harness-compat-ci.md); ADR 0015, 0019, 0028

## Context

CrossTalk depends on undocumented surfaces of two fast-moving CLIs: Claude Code's session registry and peer socket, and Codex's app-server protocol and local queue. Nothing re-checks them when a new version ships. Codex published four new minors in the 14 days after the queue wake was measured at 0.167.0, and Claude Code ten versions in the same period (npm registry, 2026-11-20).

The queue wake (ADR 0028) is gated per write on a compiled-in major.minor allowlist (`QueueTestedMinors = ["0.167"]`) plus the queue's schema digest and migration checksums. At Codex 0.171.0 a no-token run creates a `queue_1.sqlite` whose digest and migrations equal the 0.167.0 pins, yet the allowlist turns the route off. A compiled-in list reaches users only in a release, and release tags need the operator.

Operator rulings, 2026-11-20: detection is automatic and scheduled, checks run only on a changed version; a pass bumps the tested version with no human review, a fail opens an issue; Linux runners by default, macOS where coverage needs it (standard hosted runners are free for public repositories); models through OpenRouter keys held as repo secrets, never exposed to fork pull requests, spend capped.

## Decision

- A scheduled workflow, `harness-compat.yml` (daily plus `workflow_dispatch`, no pull-request trigger), compares the npm `latest` version of each harness with `.github/harness-compat.json` and runs checks only for a changed one. Jobs (operator rulings 2026-11-21): `check-updates` ("Check for updates": the version comparison, formerly `detect`); `compare-<harness>` ("Compare <harness> <version> to what CrossTalk expects", formerly `tier0-<harness>`: no tokens, install, `install --check`, the Codex version format and queue schema, the Claude registry file and socket) on both Linux and macOS for both harnesses, because the mechanism can differ by platform; `build` (one build, a per-OS artifact `crosstalk-bin-<os>`); four live legs `harness-claude-linux`, `harness-claude-macos`, `harness-codex-linux` and `harness-codex-macos`, each running the same shared test (a same-harness session asks a same-harness session, pass only on a `reply` receipt) plus OS extras: `harness-codex-macos` adds the queue-wake acceptance with a Codex sender, so `delivery_mode` is read from Codex's own view, an idle `codex --no-daemon` session woken by a peer, including an interrupted thread staying paused; and `report` (one result per harness for the bump and issue jobs, and the one OpenRouter usage read of the run). There is no `cross` job: its only unique coverage was the environment a harness gives its MCP server, and every discovery path derives from `$HOME`, so each leg instead asserts that the shim the harness launched resolved the leg's scratch `HOME` (its descriptor exists under `<scratch>/.local/state/crosstalk/agents/`), a per-host check with no peer sessions and no product change. A harness passes on its own legs, so a new harness is one more set of legs and one break does not hide behind another. `harness-codex-macos` runs, and is required, whenever Codex is tested. Until plan step 4 removes the queue gate's version allowlist it pins `queue_codex_version` 0.167.0 (a dispatch input default), and the schedule waits for that step. A job that does nothing must read skipped or failed in the Actions UI, never success: gating is a job-level `if:` or a step that exits non-zero, not an in-step output that later steps skip on.
- OpenRouter usage is read once per run, in `report`; `key-before` and `key-after` are removed. Usage lags spend, so a second read in the same run never added information; a run's cost is the next run's read minus this one's.
- Checks assert CrossTalk state (journal, `list_agents`, exit codes), never model prose; a live message passes only when its receiver calls `reply` on its id.
- On pass, a separate job in a `main`-only environment `harness-compat-bump` holds the release App token (ADR 0019; its `homebrew-tap` environment is tag-only), bumps only `.github/harness-compat.json`, and lands it through `land-pr` bound to the exact PR number, head repository, head commit and a record-only diff; required checks gate the merge. On fail, the workflow's `GITHUB_TOKEN` opens or reuses one issue per (harness, version), with no session or host detail.
- The OpenRouter key is an environment secret of environment `harness-compat`, deployable from `main` only, with an OpenRouter credit limit; jobs that run harness code never hold the App key, write no cache and persist no credentials, and the job holding the App key runs no harness code.
- The queue gate stops checking a major.minor allowlist. It keeps the schema digest and migration checks inside the write transaction, the `codex --version` parse, and a version floor of 0.167.0. CI is the watchdog for behaviour changes the schema cannot show. This holds only if the macOS run covers the interrupted case reliably; otherwise the allowlist stays and the bump adds to it.

## Rejected alternatives

- **Auto-bump the allowlist, auto-merge, auto-tag a patch release.** Tags a release every few days without the operator, which this repo forbids, and users stay blocked until they upgrade CrossTalk.
- **Signed allowlist fetched at runtime.** Adds network calls from a local shim, a signing key to hold, an offline behaviour and a remote switch over local sessions.
- **Keep the allowlist and bump it by PR only.** Every Codex minor turns the experimental route off for users until a release; the product stops on every harness release.
- **Run everything on Linux.** The queue-wake owner proof fails closed on typical Linux hosts and the route is macOS-only in v1, so a Linux-only run cannot cover it.
- **Run on pull requests too.** Would expose the model key to fork code or require `pull_request_target`; harness versions do not change in a PR.

## Consequences

- A behaviour-breaking Codex release with an unchanged schema can reach users of the experimental wake before a fix ships; the issue the job opens and the next operator-tagged release are the remedy, and ADR 0028's bounds (fixed text, one open wake per thread) limit the effect.
- A schema change still fails closed for users without any release; updating the pins needs the step-0 style probes again.
- The operator creates the environment, the capped OpenRouter key and the label before the live legs run.
- ADR 0028 decision 5 and `codex-no-daemon-queue.md` section 7 change when the gate change is built.

## Trial record

- Council: none sat; one-shot Codex review recorded in the plan's "Codex review" section.
- Rests on: ADR 0019 (App token, `land-pr`), ADR 0028 (queue gate).
- Commits: record, detect and tier 0 (#158); the Linux round trip (#162, #167, #169); the leg matrix (#176 to #182); the restructure to `check-updates`, `compare-<harness>`, four legs and a single usage read is in progress.
- Pinning tests: planned in the plan's section 5.
