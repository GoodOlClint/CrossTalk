# Codex app-server health (#113): measurement and change plan

Status: plan for operator review. No code is changed by this document. Branch `plan-113-codex-health`, base `main` at 063358c.

## Problem

On 2026-11-09 the managed Codex app-server daemon (0.167.0) lost network access to its model API. Daemon-hosted Codex TUIs hung on "Reconnecting... waiting for network". CrossTalk still listed those sessions `reachable: true`, and `send_message`/`ask` to them returned `accepted`, but no answer ever came. `codex app-server daemon restart` fixed it. The issue's direction: a health field on `list_agents` rows for Codex sessions, a hint on `send_message`/`ask` results when the hosting app-server is unhealthy, and possibly a `crosstalk install --check` line.

## Existing state (mapped)

- CrossTalk talks to an app-server only over short-lived connections: `Adapter.connect` dials, sends `initialize`, makes one or two calls and closes (`internal/harness/codex/codex.go`, `discovery.go`, `inject.go`). It never holds a connection open and never subscribes to a thread (no `thread/resume`).
- `reachable` for a Codex row is `statusOf(thread/read)`: loaded and `canAcceptDirectInput == true` (`codex.go` `statusOf`, design §3).
- `Inject` maps `turn/start` with `toolOutput` success to `accepted` and `thread/inject_items` success (`{}`) to `queued` (`inject.go`, design §8 States, ADR 0007).
- Discovery: `LoadedThreads` walks every known endpoint (managed control socket, `CROSSTALK_CODEX_ENDPOINTS`, descriptor `harness_endpoint`s) and returns one `LoadedThread` per thread; `Harness.Discover` turns those into `harness.Session` rows; `mcpserver.listAgents` builds `Row` (`internal/mcpserver/discovery.go`).
- The send path already adds Codex-specific hints from the shim: `hintCodexNoEndpoint` for a registered Codex target with no endpoint (`internal/shim/send.go` `deliver`), joined with the ADR 0014 nudge by `joinHint`.
- `crosstalk install --check` runs K1-K7; the Codex harness contributes K1, K2, K3, K5, K6 (`internal/harness/codex/install.go` `Checks`), K7 is warn-only (`cmd/crosstalk/install.go`).
- Relevant ADRs: 0003 (discovery from registries), 0007 (toolOutput delivery), 0008 (harness behaviour stays in the harness package), 0014 (hint on the send result, refuses nothing). No ADR covers harness health. Canonical pipeline rule: harness-specific behaviour lives in the adapter; no harness branching elsewhere.

## Measurement

### Method

A private `codex app-server` 0.167.0, started by this session on its own transport (stdio, then a private unix socket under `/tmp`), with its network blocked for that process only by `HTTPS_PROXY`/`HTTP_PROXY`/`ALL_PROXY` set to a closed local port (`http://127.0.0.1:9`), and `NO_PROXY` empty. It shared `~/.codex` (auth, config) read-only in effect, because a separate `CODEX_HOME` with a copied `auth.json` could rotate the refresh token and break the real login. Threads were started with `ephemeral: true`, so nothing was written to the user's history. The shared daemon was not touched, and `~/.codex/config.toml` was not edited. A baseline ran the same calls with the network intact. Raw frames and the private server's stderr were recorded as JSONL.

The blocked server's stderr reproduced the incident's lines exactly: `failed to connect to Responses WebSocket`, `error sending request for url (https://chatgpt.com/backend-api/...)`, `stream connection failed; waiting to retry`. So the provoked state is the incident state, as far as the app-server is concerned.

Two connections were used in the second run: connection A started a thread and a turn (the role of the TUI); connection B was a fresh connection that only did what CrossTalk does (`initialize`, `thread/read`, then the two delivery calls).

### Results

| Observation | Network up | Network blocked | Signal? |
|---|---|---|---|
| `initialize` | ok, 0.16 s | ok, 0.16 s | no |
| `account/read` | ok, 1.0 s | ok, 0.4 s (cached account) | no |
| `model/list` | ok, 0.05 s | ok, 0.05 s (cached list) | no |
| `account/rateLimits/read` | ok, 0.69 s | JSON-RPC error `-32603` "failed to fetch codex rate limits: error sending request for url (https://chatgpt.com/backend-api/wham/usage)", 0.00-0.06 s | **yes** |
| `thread/read` on the stuck thread (connection B) | n/a | `status: {type: active}`, `canAcceptDirectInput: true` (CrossTalk would say reachable, busy) | no |
| `thread/inject_items` (connection B) | n/a | `{}` in 0.01 s (CrossTalk: `queued`) | no |
| `turn/start` with `toolOutput` (connection B) | n/a | a turn object in 0.01 s (CrossTalk: `accepted`) | no |
| `error` notifications (connection A, owns the thread) | none | `codexErrorInfo: responseStreamDisconnected`, `willRetry: true`, "Reconnecting... 2/5" to "5/5" within 6 s, then "Reconnecting... waiting for network" every 8-43 s, forever | yes, but only to a subscribed connection |
| `error` notifications (connection B, unsubscribed) | none | none in 35 s | no |
| `warning` notifications at connect | none | "Codex could not update marketplace ..." (git to github.com) on both connections | no: startup-only, about github, not the model API |
| `turn/completed` | n/a | never arrived in 150 s; the turn stays `inProgress` | too slow, ambiguous with a long turn |

Black-hole variant (proxy at the non-routable `10.255.255.1:9`): `account/rateLimits/read` still failed in 0.05 s with the same error. A DNS stall or a slow captive portal was not measured; the probe needs its own short timeout for that case (below).

What was not measured: an app-server using an API key (not ChatGPT auth), an app-server using a custom `model_provider` (another base URL), a partial outage where `chatgpt.com/backend-api/wham/usage` answers but the Responses endpoint does not, and the daemon itself (deliberately untouched).

## Candidate signals, ranked

| Rank | Signal | Cost | False positives | Latency | Daemon + `CROSSTALK_CODEX_ENDPOINTS` |
|---|---|---|---|---|---|
| 1 | `account/rateLimits/read` on the connection CrossTalk already opens, classified by error text | one RPC per endpoint (0.7 s healthy, 0.06 s failing), cached | API-key or custom-provider servers (method fails or measures the wrong host); a usage endpoint outage alone | immediate | both: any endpoint CrossTalk can dial |
| 2 | Subscribe to the thread (`thread/resume`) or hold a long-lived connection and read `error` notifications with `willRetry` and `responseStreamDisconnected` | a persistent connection per endpoint, plus `thread/resume` side effects on someone else's thread | low; per thread, exact | 2-12 s after a turn starts; nothing while the thread is idle | both, but only for threads with a running turn |
| 3 | After a delivery, poll the turn: still `inProgress` with no items after N seconds | extra calls per send; `thread/turns/list` refused on ephemeral threads | high: a long turn looks identical | tens of seconds | both |
| 4 | Thread status (`systemError`) or `canAcceptDirectInput` | free (already read) | n/a | n/a | measured: does not change, so no signal |
| 5 | `~/.codex/app-server-daemon/daemon.stderr.log` (last resort) | file read + text matching of an unversioned log format | stale lines after recovery unless timestamped | immediate | daemon only; useless for other endpoints |

Recommendation: rank 1, `account/rateLimits/read`. It is the only measured signal that a fresh, unsubscribed connection (what CrossTalk has) can see while the thread is idle or stuck, it costs one call on a connection already open, and it works for every endpoint kind. Rank 2 is the accurate per-thread signal but needs a persistent subscriber, which CrossTalk's design (no daemon, short connections) does not have; it is the escalation path if rank 1 proves too noisy. The daemon log is not needed: rank 1 works.

## Change plan

### 1. Probe (Codex harness package only, ADR 0008)

- `appserver.Client.RateLimits(ctx)` calls `account/rateLimits/read` with `{excludeResetCreditDetails: true}` (background-poll mode per the schema) and returns the raw error.
- `codex.classifyHealth(err) string`: `nil` → `ok`; an `*RPCError` whose message contains `error sending request` or `Connection failed` → `api_unreachable`; anything else (other RPC errors, auth-mode refusals, timeout, ambiguous) → `unknown`.
- Gate: only probe when the `initialize`d server's account is ChatGPT auth (`account/read` → `account.type == "chatgpt"`), because `wham/usage` is the ChatGPT backend. Otherwise `unknown`. (Open Question 2 covers custom providers.)
- Bound: probe timeout 2 s (`HealthProbeTimeout`); a timeout yields `unknown`, never `api_unreachable`.
- Cache: per normalized endpoint, in the Adapter, 30 s TTL (`HealthTTL`), so a `list_agents` followed by a send costs one probe. `ok` and `api_unreachable` are cached; `unknown` from a timeout is cached for the TTL too, so a stalled server does not add 2 s to every call.
- Where it runs: `LoadedThreads` probes each endpoint once on the connection it already holds, concurrently with the `thread/read` calls (the client multiplexes calls by id). `Adapter.Health(ctx, endpoint)` serves the send path from the cache, opening a connection only on a miss.

### 2. Schema (`list_agents` row)

- `harness.Session` gains `Health string`; `mcpserver.Row` gains `Health string \`json:"health,omitempty"\``. Set from `LoadedThread.Health` for Codex sessions; empty (omitted) for Claude and Antigravity rows, so their output is unchanged.
- Values: `ok`, `api_unreachable`, `unknown`.
- `reachable` keeps its meaning (the thread accepts direct input). A row can be `reachable: true, health: api_unreachable`: messages are delivered into the thread and the session cannot act on them.
- A Codex row from the descriptor directory only (thread not loaded on any endpoint) has no health; the field is omitted.
- Remote rows (`RemoteRow`, ADR 0022) do not get the field in this change; the gateway allowlist struct is untouched (Open Question 4).
- Design §3 diff (described, not applied): in the Discovery paragraph's row list, after `reachable: bool (...)`, add "`health` (Codex rows only: `ok`, `api_unreachable` when the hosting app-server's `account/rateLimits/read` fails to reach its backend, `unknown` when that cannot be told; cached 30 s per endpoint)".
- Design §8 diff (described, not applied): in the `list_agents()` bullet add `health` to the listed fields; in the hint bullet add "A send to a Codex session whose app-server is `api_unreachable` keeps its state and carries a hint that the session cannot act on it until its app-server reaches its API again."

### 3. Hint on `send_message` / `ask`

- Fires when the target is a Codex session (registered descriptor with `harness_endpoint`, or a native Codex target with a binding endpoint), the delivery state is `accepted`, `queued` or `unknown`, and `Health(endpoint)` is `api_unreachable`. It does not fire on `ok` or `unknown` health, on `rejected`/`unreachable` states, or for non-Codex targets.
- It changes no state and refuses nothing (same stance as ADR 0014); it is joined with any other hint by `joinHint`.
- Text (`hintCodexAPIUnreachable`): "That Codex session's app-server cannot reach its model API right now, so the message reached the thread but the session cannot act on it, and an ask will get no reply until the network returns. Do not resend. Tell your user; restarting the Codex app-server (codex app-server daemon restart) recovered it once, and it interrupts running Codex work."
- Placement: the two Codex hint sites in `internal/shim/send.go` `deliver` (registered branch next to `hintCodexNoEndpoint`, native branch next to `hintNativeOneWay`). The shim calls a harness-provided check rather than branching on Codex itself: an optional `harness.HealthChecker` interface (`Health(ctx, endpoint) string`) on the harness, looked up by the target's harness name. The existing `d.Harness == Codex` check next to `hintCodexNoEndpoint` is precedent, but the new code should not add more of it.

### 4. `crosstalk install --check`

- New warn-only check `K8` from the Codex harness `Checks`: dial the managed control socket; if no daemon is listening, K8 passes with detail "no managed Codex app-server running"; otherwise run the probe and report `ok` ("Codex app-server reaches its model API") or warn with "Codex app-server at <socket> cannot reach its model API: <error>. Restart it with codex app-server daemon restart." Endpoints from `CROSSTALK_CODEX_ENDPOINTS` are probed the same way, one line each.
- Warn-only, so a laptop offline at install time does not fail the install.

### 5. Tests that pin it

1. `classifyHealth` table test: the measured `-32603 ... error sending request for url (...)` error → `api_unreachable`; success → `ok`; `-32600` and an unrelated `-32603` text → `unknown`; `ErrAmbiguous`/context deadline → `unknown`. The error frame is checked in as a fixture under `internal/harness/codex/testdata/` (it carries no account data; the healthy response does, so it is not checked in).
2. Adapter test against the `appservertest` fake: rateLimits error → `LoadedThread.Health == api_unreachable`; success → `ok`; a non-ChatGPT `account/read` → `unknown` with no rateLimits call; a server that never answers rateLimits → `unknown` within `HealthProbeTimeout` and the listing still returns.
3. Cache test: two `Health` calls inside the TTL make one RPC; after the TTL, two.
4. `mcpserver` golden test: a Codex row carries `health`, a Claude row has no `health` key.
5. Shim tests: registered Codex target, `accepted`, `api_unreachable` → hint present and state still `accepted`; same with `ok` → no hint; `unreachable` state → no health hint; hint joins with the nudge at the 30th send.
6. Install check test: K8 warns on the fake's error, passes with no daemon, passes on `ok`.
7. Manual live check (not CI): the probe runbook in this plan's Measurement section, against a private blocked app-server, showing `health: api_unreachable` in `list_agents` from a CrossTalk-bound session whose `CROSSTALK_CODEX_ENDPOINTS` names the private socket, and the hint on a send to it.

Each test fails before the change and passes after; test 7 is the end-to-end evidence for the PR body.

### 6. ADRs

- New provisional ADR (next free number at implementation time; the #114 work on another branch may take 0025): "Codex host health from `account/rateLimits/read`, shown as a `list_agents` field and a send hint, never a refusal". Records the measurement, the ranking, and the rejected alternatives (subscription, turn polling, daemon log).
- ADR 0003 (discovery): edit in place to name the `health` field on Codex rows (all ADRs are provisional, AGENTS.md).
- ADR 0014: one sentence noting the send result now carries a second, non-refusing hint kind.
- AGENTS.md Architecture: one clause naming the health field and the ADR.

### Sequencing

1. Wait for the #114 branch (shim send path, design §4/§8, faq, an ADR) to merge; this change touches `internal/shim/send.go` and design §8 too.
2. PR 1: probe, classifier, cache, `list_agents` field, design §3, ADRs, tests 1-4.
3. PR 2: send hint, design §8, test 5.
4. PR 3: install K8, `docs/setup.md` line, test 6. Test 7 run live before PR 2 merges.

Estimate: PR 1 about half a day for a Sonnet worker; PR 2 and PR 3 about two hours each.

## Open Questions (for the operator)

1. Field name and values: `health: ok | api_unreachable | unknown` (proposed), or a boolean `api_reachable`, or fold it into `status`? A boolean cannot say "unknown".
2. Custom model providers and API-key auth: the probe only knows the ChatGPT backend. Accept `unknown` for those servers (proposed), or also try `model/list` with a refresh, which was measured as cached and therefore useless?
3. Should an `api_unreachable` target change the send outcome (for example `ask` returns `rejected/harness_offline` instead of `accepted`), or stay hint-only as the issue comment says (proposed: hint-only; the message is in the thread and will be acted on if the network returns before the turn gives up)?
4. Remote rows: should a paired host's gateway export `health` for its Codex rows (needs the ADR 0022 allowlist widened)? Proposed: not in this change.
5. Probe cost: one `account/rateLimits/read` per endpoint per 30 s, only while someone runs `list_agents` or sends to a Codex session. Is a call to OpenAI's usage endpoint from CrossTalk acceptable (it is the same call the Codex TUI makes in the background), and is 30 s the right TTL?
6. Should the hint name `codex app-server daemon restart` at all, given it interrupts every running Codex turn on that daemon?

## Review disposition

(Filled after the cross-model review.)
