# Plan: re-validate CrossTalk when Codex or Claude Code releases (harness-compat CI)

ADR: [0029](../decisions/0029-harness-releases-are-re-validated-by-a-scheduled-ci-job-and-the-queue-gate-trusts-the-schema-not-a-version-list.md) (Provisional). Status: design only, no code or workflow in this change. Written 2026-11-20.

## 1. Goal and operator rulings

Goal: when Codex or Claude Code (later other harnesses) publishes a new version, CrossTalk is re-checked against it without a person starting the run, and the product does not stop working on every harness release.

Operator rulings (2026-11-20), taken as given:

1. A scheduled GitHub Actions job detects new versions of the npm packages `@openai/codex` and `@anthropic-ai/claude-code` and runs checks only when a version changed.
2. On pass the tested version is bumped automatically, with no human PR review. On fail an issue is opened and nothing is bumped.
3. Linux runners by default. Amended 2026-11-20 (relayed by the coordinator): Linux was a cost choice; standard macOS runners are free for public repositories (section 3, b), so a macOS job is used wherever coverage needs it, such as the queue wake.
4. Models are reached through OpenRouter with API keys held as secrets of this repo, used only on `main`, `schedule` and `workflow_dispatch`, never by a fork pull request, with spend capped.

## 2. Existing state

- The Codex queue wake (ADR 0028, experimental, opt-in, macOS only) is gated per write by `QueueGate` in `internal/harness/codex/queuegate.go`: the parent Codex version must have a major.minor in `QueueTestedMinors` (today `["0.167"]`), the `sqlite_master` digest must equal `queueSchemaSHA256`, and `_sqlx_migrations` must equal `queueMigrations`. A version outside the list turns the route off with a hint "until the probes are re-run for that version" (plan `codex-no-daemon-queue.md`, section 7).
- The version is read by `ParentVersion`, which runs the parent's executable with `--version` and parses `^codex-cli (\d+\.\d+\.\d+)\s*$`.
- The Claude adapter has no version pin. It depends on `~/.claude/sessions/<pid>.json` (it reads `pid`, `sessionId`, `messagingSocketPath`, `kind` and more, from `os.UserHomeDir()`, not `CLAUDE_CONFIG_DIR`) and on Claude's two-line peer protocol over `/tmp/cc-socks/<pid>.sock`.
- `ci.yml` runs build, `go test -race`, vet and gofmt on `ubuntu-latest` and `macos-latest`, a Linux two-container remote e2e, and a weekly `govulncheck`. No job installs Codex or Claude Code.
- `release.yml` (ADR 0015, 0019) has the pattern this plan reuses: a job in environment `homebrew-tap` mints a token from the release GitHub App, pushes a branch and calls `scripts/release-build.sh land-pr`, which opens or reuses the PR, enables auto-merge, and falls back to a direct squash merge once GitHub reports `CLEAN` (required checks passed).
- `main` is protected by a ruleset requiring `test (ubuntu-latest)` and `test (macos-latest)`. Release tags need the operator (AGENTS.md).

Release cadence, measured from the npm registry on 2026-11-20 (`npm view <pkg> time`): Codex published 10 versions from 0.165.0 (2026-10-27) to 0.171.0 (2026-11-20), four new minors (0.168 to 0.171) since 0.167.0 on 2026-11-06; Claude Code published 10 versions from 2.1.319 to 2.1.329 since 2026-11-06. The installed Codex on the authoring host is 0.171.0, so the queue wake is already off there by the version gate.

## 3. Answers to the kickoff questions

### a. Can both CLIs run against OpenRouter?

Yes for both, with limits.

- **Claude Code.** `ANTHROPIC_BASE_URL` points Claude Code at a gateway ("`ANTHROPIC_BASE_URL` is the variable that points Claude Code at the gateway", https://code.claude.com/docs/en/llm-gateway); `ANTHROPIC_AUTH_TOKEN` is sent as `Authorization: Bearer` (https://code.claude.com/docs/en/env-vars). OpenRouter documents the setup: `ANTHROPIC_BASE_URL=https://openrouter.ai/api`, `ANTHROPIC_AUTH_TOKEN=$OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY=""`, and model slugs in `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` (https://openrouter.ai/docs/guides/guides/claude-code-integration). Limits: Anthropic "doesn't support routing Claude Code to non-Claude models through any gateway" (llm-gateway page) and OpenRouter says Claude Code "is only guaranteed to work with the Anthropic first-party provider", so the Claude leg uses an Anthropic model through OpenRouter.
- **Codex.** A `model_providers.<id>` entry with `base_url`, `env_key` and `wire_api` (config reference, https://developers.openai.com/codex/config-reference, which redirects to the ChatGPT docs host). OpenRouter serves an OpenAI-compatible Responses API at `https://openrouter.ai/api/v1/responses` (https://openrouter.ai/docs/api_reference/responses/overview.md); it is stateless and rejects `store: true` or a non-null `previous_response_id` with a 400.
- **Local probe (Codex 0.171.0, no tokens):** with `CODEX_HOME` in a scratch directory, `codex exec -c 'model_providers.openrouter={name="OpenRouter",base_url="http://127.0.0.1:47998/api/v1",env_key="OR_KEY",wire_api="responses"}' -c model_provider=openrouter -m openai/gpt-5 hi` sent `POST /api/v1/responses` to a local listener. With `wire_api="chat"` Codex refused to start: "`wire_api = "chat"` is no longer supported ... set `wire_api = "responses"`". So Codex reaches OpenRouter only through the Responses API.
- **Local probe (Claude Code 2.1.329, no tokens):** `claude -p hi` with `ANTHROPIC_BASE_URL=http://127.0.0.1:47999` and a dummy `ANTHROPIC_AUTH_TOKEN` started without a login prompt and reported `status: busy` in its session file while the listener held the connection.
- **Not yet known (first live run, section 5 step 2):** whether Codex on a non-OpenAI provider sends `store: true` or `previous_response_id`, which OpenRouter's stateless Responses API rejects. If it does, the Codex leg needs a different provider route and the plan comes back to the operator.

### b. Which step-0 probes can a Linux job cover, and which need macOS?

GitHub docs: "Use of the standard GitHub-hosted runners is free and unlimited on public repositories" (https://docs.github.com/en/actions/reference/runners/github-hosted-runners); the billing page lists "Standard GitHub-hosted runners in public repositories" as free and says "Larger runners are billed for public repositories" (https://docs.github.com/en/billing/concepts/product-billing/github-actions). Standard macOS labels include `macos-latest`, `macos-15` and `macos-26` (arm64). So `macos-latest` costs no minutes here; larger runners are not used.

Split of the step 0 probes (`codex-no-daemon-queue-step0.md`) and the step 0b items (`codex-no-daemon-queue-step0b.md`):

| Probe | What it checks | Runner | Tokens | In CI? |
|---|---|---|---|---|
| Schema digest and migrations (the gate's own check) | `queue_1.sqlite` layout and `_sqlx_migrations` | Linux | none | yes, tier 0 |
| `codex --version` format | `ParentVersion`'s regex still parses | Linux | none | yes, tier 0 |
| 1 idle, 3 interrupted, 7 two rows, 10 ownership, 12 hook `session_id`, 16 dispatch after a turn | Codex's own queue watcher and hooks, platform-neutral Rust | macOS (with the product path, below) | yes | 1, 3, 7, 12 in the macOS acceptance run (3 by sending Esc through the pty mid-turn: the row stays queued and no turn starts while interrupted, then it dispatches after the next prompt); 10 and 16 not repeated |
| 2 busy, 5 compaction | dispatch waits for the turn | macOS | yes, longer turns | no (cost; covered by manual re-probe on a schema change) |
| 6 kill -9, 8 composer draft, 9 operator-queued prompt | need a person at the TUI or a crash at an exact moment | none | yes | no; re-run by hand only when the schema digest changes |
| 11 subagent threads, 15 fetched text as data | model-dependent, single-trial evidence | none | yes | no |
| 13 lock and process facts, 14 tab-open discovery | `flock`, `proc_info` opener list | macOS | none | already covered by `internal/ownerproof` tests in `ci.yml`'s macOS leg; re-run against a live Codex in the macOS acceptance run |
| 0b items 1 to 5, 7, 8 | CrossTalk's own model of generations, mailbox and routes | either | none | already in `go test` (product code since steps 1 to 6) |
| 0b item 6 real-kernel owner proof | `flock` plus openers on a real kernel | macOS | none | already in `go test` on `macos-latest` |

The queue-wake acceptance (a real `codex --no-daemon` session, the SessionStart claim, the owner proof, a wake row, `receive_message`) needs macOS: on Linux the owner proof normally fails closed because same-user non-dumpable processes hide `/proc/<pid>/fd` (plan `codex-no-daemon-queue.md` 3.3), and the route is macOS-only in v1. Everything else runs on Linux.

### c. Does a headless Claude Code run register the way CrossTalk needs?

Yes on macOS, without a pty. Local probe, Claude Code 2.1.329: `claude -p hi` started in the background with stdout redirected (no terminal), `CLAUDE_CONFIG_DIR` set to a scratch directory and the API pointed at a local listener. Within 8 s it wrote `sessions/<pid>.json` with `kind: "interactive"`, `entrypoint: "sdk-cli"`, `peerProtocol: 1` and `messagingSocketPath: "/tmp/cc-socks/<pid>.sock"`, and the socket existed.

Consequences for the job:

- The registry follows `CLAUDE_CONFIG_DIR`, but CrossTalk's Claude adapter reads `~/.claude/sessions` from `os.UserHomeDir()`. The job sets `HOME` to a scratch directory instead of `CLAUDE_CONFIG_DIR`, so both agree.
- `claude -p` exits after its turn. A receiving session must stay alive to be messaged; the job keeps it open with `--input-format stream-json` on a held stdin. Whether a peer message that arrives on the socket starts a turn in that mode is not yet measured; section 5 step 2 measures it, and the fallback is a pty (`script`), which needs no new dependency.
- A journaled send does not prove a Claude turn: `readAck` in `internal/harness/claude/claude.go` counts a socket that stays silent through the grace period as `accepted`. Receipt is proved only by the receiving session calling `reply` on that message id (section 4.2).
- Linux was not probed locally. The tier 0 job checks the same file and socket on `ubuntu-latest` before any token is spent.

### d. Release implication: how does a bump reach users?

A version list compiled into Go reaches users only in a release, and release tags need the operator. Three options:

1. **Auto-bump PR, auto-merge, automatic patch release.** Keeps the version list as the gate. At the measured cadence (four new Codex minors in 14 days) it would tag a CrossTalk release every few days without the operator, which AGENTS.md forbids (release tags need the operator's yes), and users stay blocked between a Codex release and their own CrossTalk upgrade anyway. Rejected.
2. **The gate trusts the schema digest and migrations, not a version list; CI is the behaviour watchdog.** The gate already re-reads the schema and migrations inside every write transaction; those are what the write depends on. The version list adds a check that blocks identical schemas: at 0.171.0 the probe in this plan created `queue_1.sqlite` with zero tokens and its digest (`7def6f4e…`) and migrations equal the 0.167.0 pin, yet the gate refuses it. A schema change still fails closed with no release. What the schema cannot catch, a change in the watcher's behaviour with the same layout, is what the CI acceptance run is for; it opens an issue on failure. The automatic bump then records the tested version in a data file on `main` that no release has to carry.
3. **Signed allowlist fetched at runtime.** Fastest to reach users, but it adds outbound network calls from a local-only shim, a signing key to hold and rotate, an offline behaviour to define, and a remote switch over a user's local session; none of that exists today. Rejected.

**Recommendation: option 2, conditional on the macOS leg covering the interrupted case (section 3 b).** If that check cannot be made reliable, the version allowlist stays and the bump PR adds to it, accepting that the route stays off between a Codex release and the next CrossTalk release. Otherwise the gate keeps the schema digest, the migrations, a version floor (`>= 0.167.0`, the first measured version, so an older Codex without the revision triggers stays off) and the `ParentVersion` parse; the major.minor allowlist is removed. The tested versions live in `.github/harness-compat.json`, which the job bumps on pass (ruling 2). A behaviour failure with an unchanged schema opens an issue; the fix (a code change, a denylist entry or a new digest) ships in the next release the operator tags. Trade-off accepted: between a behaviour-breaking Codex release and that fix, users who opted into the experimental wake can get a wrong wake; the schema gate and the ADR 0028 bounds (fixed text, one open wake per thread) limit what a wrong wake can do.

### e. How does the bump PR merge with no human?

Reuse ADR 0019's App and `land-pr`, not its environment: `homebrew-tap` allows only `v*` tags (`docs/plans/release-0.1.4.md` section 9), so a scheduled run on `main` cannot enter it. The `bump` job runs in a new environment `harness-compat-bump`, deployable from `main` only, holding a copy of the App's client id and key (or the key of a second App installed on CrossTalk alone), separate from the model-key environment. It mints the App token (`actions/create-github-app-token`, Contents and Pull requests write), pushes branch `harness-compat-<harness>-<version>` and calls `scripts/release-build.sh land-pr GoodOlClint/CrossTalk <branch> <title>`. Because the App token opens the PR, `ci.yml` runs on it and the ruleset's required checks gate the merge; `land-pr` enables auto-merge or merges directly on `CLEAN`, and fails on a failed required check. `land-pr` today picks the first PR whose head branch name matches and checks neither the head repository nor the commit, so a fork PR with the same branch name could be selected. The bump path therefore carries the exact PR number from creation, and before merging verifies head repository = `GoodOlClint/CrossTalk`, base `main`, head commit = the commit it pushed, and a diff of only `.github/harness-compat.json`; it merges with `--match-head-commit`. On a rerun it succeeds early only if `main`'s record already holds the intended version. This is a `land-pr` change (or a new `land-bump` subcommand) with its own test, and the marketplace job benefits from it too. The issue on failure is opened with the workflow's own `GITHUB_TOKEN` (`permissions: issues: write`), so the App needs no new permission.

## 4. Design

### 4.1 Record

`.github/harness-compat.json`, one entry per harness: `{"codex": {"tested": "0.171.0", "checked": "2026-11-20"}, "claude-code": {"tested": "2.1.329", "checked": "2026-11-20"}}`. Every entry starts as `"tested": "unverified"` (section 5 step 1) and gains a version only from a passing run. It is the job's memory of what passed. No Go code reads it under option 2; `docs/setup.md` links it as "versions last checked".

### 4.2 Workflow `harness-compat.yml`

Triggers: `schedule` (daily) and `workflow_dispatch` (inputs: harness, version, force). No `pull_request` or `pull_request_target` trigger, so no fork PR can run it. `concurrency: harness-compat`, no cancel.

| Job | Runner | Environment | Secrets | Does |
|---|---|---|---|---|
| `detect` | ubuntu-latest | none | none | `npm view <pkg> version` (the `latest` dist-tag) per harness; compares with the record; outputs the changed set; nothing changed → the run ends |
| `tier0` | ubuntu-latest | none | none | per changed harness: install the exact version, build `crosstalk`, `HOME` in scratch, `crosstalk install <harness> --yes` (the installer exits 2 without a terminal or `--yes`), `crosstalk install --check` for that harness, both exit codes required; Codex: `codex --version` matches the parse, a no-token `codex exec` against a closed local port creates `queue_1.sqlite`, digest and migrations compared with the pins; Claude: `claude -p` against a closed local port writes `sessions/<pid>.json` with the fields the adapter reads and creates the socket |
| `build` | ubuntu-latest | none | none | builds `crosstalk` once, runs the harness-compat script tests, uploads the binary as an artifact (R229) |
| `harness` (matrix, `fail-fast: false`) | per leg | `harness-compat` | `OPENROUTER_API_KEY` | needs `tier0` and `build`; one same-harness leg per harness behind one script entry point keyed by harness: `claude` (ubuntu; one Claude session asks another, pass only on a `reply` receipt), `codex-linux` (ubuntu; one Codex session asks another, app-server daemon route), `codex-macos` (macos-latest, only when Codex changed and required for every Codex bump; `crosstalk install codex --codex-no-daemon-queue --yes`, a `codex --no-daemon` session under `script`, one fixed first prompt so SessionStart fires, an `immediate` inform from a verified local sender, checks: `list_agents` shows `delivery_mode: queue-wake (experimental)`, a wake row was written and consumed, the entry went `mailboxed` to `delivered`, then the interrupted case from section 3 b). The Codex legs dump `thread/read` for every loaded thread and assert a lone session shows exactly one Codex row in `list_agents`. A harness passes when its own legs pass. Adding a harness is one matrix entry |
| `cross` | ubuntu-latest | `harness-compat` | `OPENROUTER_API_KEY` | needs every `harness` leg; runs only if all passed (skipped, not failed, otherwise); one Claude and Codex round trip, an `ask` in each direction, at the tested versions |
| `report` | ubuntu-latest | none | none | `if: always()`; one line per leg in the step summary, and a machine-readable per-harness result (job output) that `bump` and the issue job read. Verdicts come from GitHub's recorded job and step conclusions (the jobs API), never from a file a runner wrote; a leg whose test step did not run (no key, tier 0 of its harness failed) reads as skipped, never as a pass |
| `bump` | ubuntu-latest | `harness-compat-bump` | release App key | needs every required job to pass; builds the record from `detect`'s outputs and the job conclusions, not from any artifact an untrusted job wrote; checks out no harness, runs no harness code; pushes and lands per section 3 e |
| `issue` | ubuntu-latest | none | `GITHUB_TOKEN` (`issues: write`) | per failing harness; one issue per (harness, version), reused if open, label `harness-compat`; body names the version, the failing step and the run URL only (public-repo hygiene) |

Checks assert CrossTalk state (journal records, `list_agents` fields, exit codes), never what the model says, and each live job runs with `--max-turns` and a step timeout. Actions are pinned to the majors this repo's workflows already use (`actions/checkout@v7`, `actions/setup-go@v7`, `actions/upload-artifact@v7`, `actions/create-github-app-token@v3`); they are newer than the global floor (checkout@v6, upload-artifact@v5) and pass on `main` today.

### 4.3 Secrets and spend

- One OpenRouter key, held as an environment secret of environment `harness-compat`, whose deployment branch policy allows only `main`. Environment secrets are secrets of this repo, scoped further; a job outside the environment cannot read the key.
- The key is created by the operator with an OpenRouter credit `limit` and `limit_reset: monthly` (https://openrouter.ai/docs/api/api-reference/api-keys/create-a-new-api-key.md). The cap is the bound; the operator picks the amount.
- The jobs that run the new, untrusted harness release (`tier0`, `live-*`) never hold the App key; the `bump` job that holds the App key runs no harness code. Those jobs also run with `permissions: contents: read`, `persist-credentials: false` and `actions/setup-go` with `cache: false`, and use no other cache action, because scheduled and dispatched runs on `main` can write default-branch caches that later CI runs restore (https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching). With that, a compromised npm release can spend at most the capped key.
- Budget (operator, 2026-11-21): the key has $10 per month. Every run logs `GET /api/v1/key` once before the legs and once after `cross` (`usage`, `limit`, `limit_remaining`). The usage figure lags the spend, so a per-leg delta reads 0; the next run's before value gives the true cost of this one. Measured, run 4 on main (Claude `anthropic/claude-haiku-5.5` and Codex `openai/gpt-6-luna` through OpenRouter, one reply round trip each way): $0.0067 per run. At the daily schedule (30 runs) that is about $0.20 per month, 2% of the budget. The matrix adds three same-harness legs; each is expected to cost about the same, so a full run is projected near $0.03 and about $1 per month, to be confirmed by the first matrix dispatch.
- Models: the cheapest tool-capable Anthropic model slug for Claude and the cheapest tool-capable OpenAI model slug on OpenRouter's Responses API for Codex, held as repo variables so they change without a code edit.

## 5. Sequencing and test bar

Each step is one PR; nothing is built before the operator approves this plan. `workflow_dispatch` only works for a workflow already on the default branch, and the live environments deploy from `main` only, so each live leg is first merged dispatch-only, proved by dispatch runs on `main`, and only then given its schedule and its bump.

1. **Record, detect, tier 0.** Add the record (all entries `unverified`) and the `detect` and `tier0` jobs, dispatch-only, no secrets. Pre-merge evidence: `go test` for the record parser and the bump guard; actionlint on the workflow. Done when a dispatch on `main` with a forced version runs tier 0 green for both harnesses, and a run with an unchanged record ends after `detect`.
2. **Live Linux round trip, dispatch-only. Built and measured (2026-11-21).** One Claude and one Codex session through OpenRouter, an `ask` each way, pass only on a `reply` receipt (PRs #162, #167, #169). Run 4 on main passed both directions with discovery in 33 s for $0.0067. The open points are answered: the Codex daemon works on Linux from the npm package, a stream-json Claude session wakes on a peer ask, and Codex on OpenRouter's Responses API ran turns and tool calls.
3. **Leg matrix (R229, operator 2026-11-21).** Replaces the single macOS slice. `build` once, then a `harness` matrix of same-harness legs behind one script entry point keyed by harness (`claude`: Claude asks Claude; `codex-linux`: Codex asks Codex; `codex-macos`: the queue-wake acceptance with the interrupted case), then `cross` (the slice 2 round trip, only when every leg passed), then `report` (a result per harness). It lands as three PRs: matrix with the `claude` and `codex-linux` legs plus `cross` and `report`; the Codex thread dump and the one-row assertion; the `codex-macos` leg. Done when a dispatch on main shows a result per leg and the cross result, a run with the setting file removed shows the macOS route off, and the interrupted case is reliable. If it cannot be made reliable, option 2 is dropped and the allowlist stays (section 3 d).
4. **Gate change (option 2), proved together with a new Codex version.** Go change in `queuegate.go`: drop `QueueTestedMinors`, add the version floor; the schema and migration checks stay. Test pinned: a version above the floor with the pinned schema passes, a version below fails, a changed digest fails. Before merge, a dispatch of the macOS leg runs a build of this branch's gate against the newest Codex. ADR 0028 decision 5 and `codex-no-daemon-queue.md` section 7 are edited in the same PR. Reaches users with the next operator-tagged release.
5. **Bump, report, schedule.** Adds the `harness-compat-bump` environment's job, the PR-binding `land-pr` change and the `report` job, then the daily schedule. Done when a forced passing version lands a merged bump PR changing only the record, a same-named branch from a fork is refused by the binding check (unit test), a forced failure opens one issue and no PR, and a second failing run for the same version reuses the issue.

## 6. Operator actions (prerequisites)

- Create environment `harness-compat` (model key) and environment `harness-compat-bump` (App client id and key), each with a `main`-only deployment branch policy.
- Create the capped OpenRouter key and store it there as `OPENROUTER_API_KEY`.
- Create the `harness-compat` label.
- Approve option 2 in section 3 d, which edits ADR 0028's version gate.

## 7. Open points and risks

- The two unknowns in 3 a and 3 c are answered by the first dispatch run, not assumed.
- Whether a plain `codex` on Linux attaches to the managed app-server daemon is unmeasured (`docs/setup.md`); the live Linux leg (slice 2b) waits for the OpenRouter key and answers it.
- The OpenRouter key sits in the job-level environment of the `harness` and `cross` jobs while the harness is installed with npm, so an install script in a compromised release could read it. Accepted for now (the key is capped and `main`-only); the fix is to scope the key to the step that runs the leg.
- The Codex legs dump `thread/read` for every loaded thread (id, status, source, ephemeral, parentThreadId, sessionId; no names, paths or text) and check that a lone Codex session's first `list_agents` shows exactly one Codex row. The check is fatal for the leg (the helper-thread fix, #171, is on main). The dump keeps ids, status type, the source category and helper subtype, never names, paths or free text.
- A harness release on npm's `latest` that is pulled back the same day can produce a bump for a version no user runs; harmless, the record only says it passed.
- The macOS leg drives a TUI through a pty. If it proves flaky, Codex bumps stop (the leg is required for them) and an issue says so; it is not retried until green, and Claude bumps continue.
- OpenRouter's model routing can change behind a slug; a live failure whose tier 0 passed is reported as such so a model-side failure is not read as a harness break.

## 8. Codex review

One-shot adversarial review by Codex on 2026-11-20 against `main` (verdict: needs-attention, 8 findings). Each finding was checked against the code before deciding.

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | critical | `land-pr` selects a PR by head branch name only, so a fork PR with the same branch name could be merged by the App | Fixed: section 3 e binds the merge to the PR number, head repository, base, head commit and a record-only diff; step 5 tests it. Verified in `scripts/release-build.sh` (`find_pr` uses `--head` only) |
| 2 | high | `homebrew-tap` is limited to `v*` tags, so a scheduled run on `main` cannot use it | Fixed: new `main`-only environment `harness-compat-bump` (sections 3 e, 4.2, 6). Verified in `docs/plans/release-0.1.4.md` section 9 |
| 3 | high | An idle wake does not show that an interrupted thread stays paused | Fixed: the interrupted case joins the macOS leg (3 b, 4.2); option 2 is conditional on it (3 d) |
| 4 | high | The rollout relaxed the gate and bumped versions before macOS coverage existed | Fixed: section 5 reordered; the record starts `unverified`; the macOS leg is required for every Codex bump |
| 5 | high | The Linux round trip can pass with Claude inbound broken, since a silent socket counts as `accepted` | Fixed: an `ask` each way, pass only on a `reply` to that id (3 c, 4.2). Verified in `readAck`, `internal/harness/claude/claude.go` |
| 6 | high | Untrusted harness jobs could write default-branch caches that later runs restore | Fixed: read-only permissions, no persisted credentials, `cache: false`, record built only from trusted outputs (4.3, 4.2) |
| 7 | medium | `crosstalk install` exits 2 without a terminal unless `--yes` | Fixed: `--yes` and a named harness in tier 0 and the macOS leg. Verified in `cmd/crosstalk/install.go` |
| 8 | medium | Dispatch runs need the workflow on the default branch, and the live environments are `main`-only | Fixed: each leg merges dispatch-only first and is proved on `main` (section 5 preamble) |
