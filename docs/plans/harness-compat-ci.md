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
| 1 idle, 7 two rows, 10 ownership, 12 hook `session_id`, 16 dispatch after a turn | Codex's own queue watcher and hooks, platform-neutral Rust | macOS (with the product path, below) | yes | 1, 7, 12 as part of the macOS acceptance run; 10 and 16 not repeated |
| 2 busy, 5 compaction | dispatch waits for the turn | macOS | yes, longer turns | no (cost; covered by manual re-probe on a schema change) |
| 3 interrupted, 6 kill -9, 8 composer draft, 9 operator-queued prompt | need a person at the TUI or a crash at an exact moment | none | yes | no; re-run by hand only when the schema digest changes |
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
- Linux was not probed locally. The tier 0 job checks the same file and socket on `ubuntu-latest` before any token is spent.

### d. Release implication: how does a bump reach users?

A version list compiled into Go reaches users only in a release, and release tags need the operator. Three options:

1. **Auto-bump PR, auto-merge, automatic patch release.** Keeps the version list as the gate. At the measured cadence (four new Codex minors in 14 days) it would tag a CrossTalk release every few days without the operator, which AGENTS.md forbids (release tags need the operator's yes), and users stay blocked between a Codex release and their own CrossTalk upgrade anyway. Rejected.
2. **The gate trusts the schema digest and migrations, not a version list; CI is the behaviour watchdog.** The gate already re-reads the schema and migrations inside every write transaction; those are what the write depends on. The version list adds a check that blocks identical schemas: at 0.171.0 the probe in this plan created `queue_1.sqlite` with zero tokens and its digest (`7def6f4e…`) and migrations equal the 0.167.0 pin, yet the gate refuses it. A schema change still fails closed with no release. What the schema cannot catch, a change in the watcher's behaviour with the same layout, is what the CI acceptance run is for; it opens an issue on failure. The automatic bump then records the tested version in a data file on `main` that no release has to carry.
3. **Signed allowlist fetched at runtime.** Fastest to reach users, but it adds outbound network calls from a local-only shim, a signing key to hold and rotate, an offline behaviour to define, and a remote switch over a user's local session; none of that exists today. Rejected.

**Recommendation: option 2.** The gate keeps the schema digest, the migrations, a version floor (`>= 0.167.0`, the first measured version, so an older Codex without the revision triggers stays off) and the `ParentVersion` parse; the major.minor allowlist is removed. The tested versions live in `.github/harness-compat.json`, which the job bumps on pass (ruling 2). A behaviour failure with an unchanged schema opens an issue; the fix (a code change, a denylist entry or a new digest) ships in the next release the operator tags. Trade-off accepted: between a behaviour-breaking Codex release and that fix, users who opted into the experimental wake can get a wrong wake; the schema gate and the ADR 0028 bounds (fixed text, one open wake per thread) limit what a wrong wake can do.

### e. How does the bump PR merge with no human?

Reuse ADR 0019 as is: the `bump` job runs in environment `homebrew-tap`, mints the release App token (`actions/create-github-app-token`, Contents and Pull requests write), pushes branch `harness-compat-<harness>-<version>` and calls `scripts/release-build.sh land-pr GoodOlClint/CrossTalk <branch> <title>`. Because the App token opens the PR, `ci.yml` runs on it and the ruleset's required checks gate the merge; `land-pr` enables auto-merge or merges directly on `CLEAN`, and fails on a failed required check. Before landing, the job checks that the branch changes only `.github/harness-compat.json` and stops otherwise. The issue on failure is opened with the workflow's own `GITHUB_TOKEN` (`permissions: issues: write`), so the App needs no new permission.

## 4. Design

### 4.1 Record

`.github/harness-compat.json`, one entry per harness: `{"codex": {"tested": "0.171.0", "checked": "2026-11-20"}, "claude-code": {"tested": "2.1.329", "checked": "2026-11-20"}}`. It is the job's memory of what passed. No Go code reads it under option 2; `docs/setup.md` links it as "versions last checked".

### 4.2 Workflow `harness-compat.yml`

Triggers: `schedule` (daily) and `workflow_dispatch` (inputs: harness, version, force). No `pull_request` or `pull_request_target` trigger, so no fork PR can run it. `concurrency: harness-compat`, no cancel.

| Job | Runner | Environment | Secrets | Does |
|---|---|---|---|---|
| `detect` | ubuntu-latest | none | none | `npm view <pkg> version` (the `latest` dist-tag) per harness; compares with the record; outputs the changed set; nothing changed → the run ends |
| `tier0` | ubuntu-latest | none | none | per changed harness: install the exact version, build `crosstalk`, `HOME` in scratch, `crosstalk install`, `crosstalk install --check`; Codex: `codex --version` matches the parse, a no-token `codex exec` against a closed local port creates `queue_1.sqlite`, digest and migrations compared with the pins; Claude: `claude -p` against a closed local port writes `sessions/<pid>.json` with the fields the adapter reads and creates the socket |
| `live-linux` | ubuntu-latest | `harness-compat` | `OPENROUTER_API_KEY` | needs `tier0`; one Claude session and one Codex session (app-server daemon route), both through CrossTalk: Claude sends an `ask` to Codex, Codex answers with `reply`, then the reverse with an `inform`; checks the journal states, not model prose |
| `live-macos` | macos-latest | `harness-compat` | `OPENROUTER_API_KEY` | needs `tier0`; only when Codex changed; `crosstalk install --codex-no-daemon-queue`, a `codex --no-daemon` session under `script` (pty), one fixed first prompt so SessionStart fires, an `immediate` inform from a verified local sender, then checks: `list_agents` shows `delivery_mode: queue-wake (experimental)`, a wake row was written and consumed, the entry went `mailboxed` → `delivered` |
| `bump` | ubuntu-latest | `homebrew-tap` | release App key | needs every live job that ran to pass; rewrites the record for the passing harnesses; checks the diff touches only the record; pushes and runs `land-pr` |
| `report` | ubuntu-latest | none | `GITHUB_TOKEN` (`issues: write`) | `if: failure()`; one issue per (harness, version), reused if open, label `harness-compat`; body names the version, the failing step and the run URL only (public-repo hygiene) |

Checks assert CrossTalk state (journal records, `list_agents` fields, exit codes), never what the model says, and each live job runs with `--max-turns` and a step timeout. Actions are pinned to the majors this repo's workflows already use (`actions/checkout@v7`, `actions/setup-go@v7`, `actions/upload-artifact@v7`, `actions/create-github-app-token@v3`); they are newer than the global floor (checkout@v6, upload-artifact@v5) and pass on `main` today.

### 4.3 Secrets and spend

- One OpenRouter key, held as an environment secret of environment `harness-compat`, whose deployment branch policy allows only `main`. Environment secrets are secrets of this repo, scoped further; a job outside the environment cannot read the key.
- The key is created by the operator with an OpenRouter credit `limit` and `limit_reset: monthly` (https://openrouter.ai/docs/api/api-reference/api-keys/create-a-new-api-key.md). The cap is the bound; the operator picks the amount.
- The jobs that run the new, untrusted harness release (`tier0`, `live-*`) never hold the App key; the `bump` job that holds the App key runs no harness code. A compromised npm release can spend at most the capped key.
- Models: the cheapest tool-capable Anthropic model slug for Claude and the cheapest tool-capable OpenAI model slug on OpenRouter's Responses API for Codex, held as repo variables so they change without a code edit.

## 5. Sequencing and test bar

Each step is one PR; nothing is built before the operator approves this plan.

1. **Record and detect.** Add the record and the `detect` and `tier0` jobs, no secrets. Done when a `workflow_dispatch` with a forced version runs tier 0 green on Linux for both harnesses, and a run with an unchanged record ends after `detect`.
2. **Live Linux leg.** Needs the operator's OpenRouter key in environment `harness-compat`. First run is `workflow_dispatch` only, to answer the two open points (Codex and OpenRouter's stateless Responses API; a stream-json Claude session receiving a peer message). Done when one run shows the ask and reply journaled in both directions, and a run with a broken key fails and opens one issue.
3. **Bump and report.** Done when a forced new version that passes lands a merged bump PR through `land-pr` with only the record changed, and a forced failure opens one issue and no PR; a second failing run for the same version reuses the issue.
4. **Gate change (option 2).** Go change in `queuegate.go`: drop `QueueTestedMinors`, add the version floor; the schema and migration checks stay. Test pinned: a version above the floor with the pinned schema passes, a version below fails, a changed digest fails. ADR 0028 decision 5 and plan section 7 are edited in the same PR. Reaches users with the next operator-tagged release.
5. **macOS queue-wake leg.** Done when one run on `macos-latest` shows the wake checks above, and a run with the setting file removed shows the route off.

Each workflow change runs `ci.yml` (it validates nothing about the new workflow); the new workflow is proved by its own dispatch runs, recorded in the PR body.

## 6. Operator actions (prerequisites)

- Create environment `harness-compat` with a `main`-only deployment branch policy.
- Create the capped OpenRouter key and store it there as `OPENROUTER_API_KEY`.
- Create the `harness-compat` label.
- Approve option 2 in section 3 d, which edits ADR 0028's version gate.

## 7. Open points and risks

- The two unknowns in 3 a and 3 c are answered by the first dispatch run, not assumed.
- A harness release on npm's `latest` that is pulled back the same day can produce a bump for a version no user runs; harmless, the record only says it passed.
- The macOS leg drives a TUI through a pty; if that proves flaky, it runs on dispatch only and the issue says so, rather than retrying until green.
- OpenRouter's model routing can change behind a slug; a live failure whose tier 0 passed is reported as such so a model-side failure is not read as a harness break.

## 8. Codex review

Pending.
