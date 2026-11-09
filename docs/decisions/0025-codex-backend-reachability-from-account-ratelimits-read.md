# ADR 0025 — Codex backend reachability from account/rateLimits/read, shown as a list_agents field and a send hint, never a refusal

- **Status:** Provisional (operator 2026-11-09)
- **Date:** 2026-11-09
- **Deciders:** operator + Claude (Sonnet 5.5) + Codex review of the plan
- **Context source:** issue #113; `docs/plans/codex-health-113.md` (measurement, ranking of signals, operator rulings 2026-11-09)

## Context

On 2026-11-09 the managed Codex app-server daemon (0.167.0) lost network access to its model API. Daemon-hosted Codex TUIs hung on "Reconnecting... waiting for network". CrossTalk still listed those sessions `reachable: true`, and `send_message` and `ask` to them returned `accepted`, but no answer came. `codex app-server daemon restart` fixed it.

`reachable` for a Codex row is `thread/read` status plus `canAcceptDirectInput` (design §3), and `accepted` and `queued` mean the app-server took the call (ADR 0007). Neither says whether the app-server can reach OpenAI. CrossTalk opens only short connections and never subscribes to a thread (ADR 0003), so it sees none of the `error` notifications the owning TUI receives.

The plan measured a private app-server whose network was blocked for that process only (a closed local proxy port), against a baseline with the network up. With the network blocked: `initialize`, `account/read` and `model/list` still succeeded from cache; `thread/read` still said `active` with `canAcceptDirectInput: true`; `thread/inject_items` and `turn/start` with `toolOutput` still succeeded in 0.01 s; a turn never completed; `error` notifications reached only the connection that owned the thread. Only `account/rateLimits/read` changed on a fresh, unsubscribed connection: it failed in under 0.1 s with JSON-RPC `-32603`, "failed to fetch codex rate limits: error sending request for url (https://chatgpt.com/backend-api/wham/usage)". The server's stderr in that state matched the incident's lines.

## Decision

Report whether the app-server behind a Codex session reaches its OpenAI backend, as what it measures and no more.

1. **Probe.** `account/rateLimits/read` (params `excludeResetCreditDetails: true`), classified by `classifyBackend` in `internal/harness/codex`: success is `reachable`; an RPC error whose message contains `error sending request` or `Connection failed` is `unreachable`; anything else (other RPC errors, timeout, ambiguous result, cancellation) is `unknown`.
2. **Gates.** An endpoint is probed only when `account/read` says the account type is `chatgpt`, because the usage endpoint belongs to ChatGPT auth; otherwise it is `unknown` with no rate-limits call. A thread takes its endpoint's result only when `thread/read` says `modelProvider` is `openai`; any other provider is `unknown`.
3. **Bounds.** The probe has a 2 s timeout (`BackendProbeTimeout`); a timeout is `unknown`, never `unreachable`. The result is cached per normalized endpoint for 30 s (`BackendTTL`), including an `unknown` from a timeout, so one `list_agents` followed by a send costs one probe. At most one probe per endpoint is in flight; probes of different endpoints run concurrently, detached from the caller, and never delay thread discovery. `LoadedThreads` hands each endpoint's connection to its probe, walks every endpoint, then collects probe results only while its context lives; a probe still running then leaves those threads `unknown` and the rows are still returned. `Adapter.Backend` serves the send path from the cache and connects only on a miss.
4. **Field.** Codex rows in `list_agents` gain `backend`: `reachable`, `unreachable` or `unknown`. Claude rows, rows with no loaded thread, and remote rows (ADR 0022) omit it. `reachable` keeps its meaning (the thread takes input), so a row can be `reachable: true, backend: unreachable`.
5. **Hint.** A send to a Codex session whose `backend` is `unreachable` keeps its state and carries a hint, joined with the ADR 0014 nudge. For `accepted` and `queued` the hint says the message is in the thread but the session may not act on it until the network returns, not to resend, and to tell the user. For `unknown` delivery it makes no delivery claim and keeps the no-blind-resend rule. Both name `codex app-server daemon restart` as advice for the model to relay to its user, never to run, and say it interrupts running Codex work. The shim finds the check through the optional `harness.BackendChecker` interface, `Backend(ctx, endpoint, sessionRef)`, with no Codex branch of its own (ADR 0008). The Codex implementation applies the provider gate of item 2 to the target thread: it reads `thread/read` of `sessionRef` only when the endpoint is `unreachable`, and returns `unreachable` only for an `openai` thread; any error is `unknown`. A send is never refused because of the backend.
6. **Install check.** `crosstalk install --check` has a warn-only check K9 from the Codex harness: no daemon listening passes ("no managed Codex app-server running"), `reachable` passes, `unreachable` warns with the restart advice, `unknown` passes and says it could not tell. (K8 is the remote check.)

## Rejected alternatives

- **Subscribe to the thread or hold a connection and read `error` notifications** (`willRetry`, `responseStreamDisconnected`). It is exact per thread, but needs a persistent subscriber per endpoint and `thread/resume` side effects on someone else's thread, and it sees nothing while a thread is idle. CrossTalk has no daemon. It is the escalation path if this probe proves too noisy.
- **Poll the turn after a delivery** (still `inProgress` with no items after N seconds). A long turn looks identical, and `thread/turns/list` is refused on ephemeral threads.
- **Thread status or `canAcceptDirectInput`.** Measured: neither changes in the outage.
- **Read `~/.codex/app-server-daemon/daemon.stderr.log`.** An unversioned log format, stale lines after recovery, and useless for any endpoint that is not the daemon.
- **Refuse the send when the backend is unreachable.** The probe is evidence, not proof, and a refused send loses a message that the thread would hold until the network returns. Operator ruling 2026-11-09: hint only.

## Consequences

- The probe tests the ChatGPT backend host (`chatgpt.com/backend-api`), not the model. A usage-only outage reports `unreachable` for a working session, and a Responses-only outage reports `reachable` for a stuck one. A proxy cannot provoke either, since both endpoints share a host, so neither was measured. A DNS stall or captive portal was not measured; the 2 s timeout covers it as `unknown`.
- API-key accounts and custom model providers are `unknown`, as is a session whose app-server cannot be dialed. The API-key and custom-provider cases were not measured against a live server.
- One extra `account/read` and one `account/rateLimits/read` per endpoint per 30 s, only for `list_agents` or a send to a Codex session.
- Remote hosts do not export `backend`; a remote sender gets no backend hint.
- Pinning tests: `internal/harness/codex/backend_test.go`, `backend_classify_test.go` (error frame fixture `testdata/ratelimits-backend-down.json`), `install_k9_test.go`, `internal/mcpserver/golden_test.go`, `internal/shim/backend_hint_test.go`. Not covered in CI: the live check in the trial record.

## Trial record

- Council verdict: none; the plan was reviewed once by Codex (adversarial review) and approved by the operator with six rulings.
- Rests on: ADRs 0003, 0007, 0008, 0014 (provisional).
- Commits: `git log --grep 'Provisional-ADR: 0025'` (none yet).
- Outcome: run live on 2026-11-09 with the branch binary's `list_agents` (Codex 0.167.0). A thread on a private app-server whose network was blocked showed `reachable: true, backend: unreachable`; a thread on the managed daemon showed `backend: reachable`. The send hint was not run live; the shim tests cover it.
