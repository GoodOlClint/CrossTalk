# ADR 0007 — Codex delivery as named toolOutput

- **Status:** Provisional
- **Date:** 2026-11-03
- **Deciders:** operator + Claude + Codex
- **Context source:** operator authorization to design and build the Codex adapter on `codex/toolout`; live 0.166.0 probes recorded in `~/Documents/Codex/2026-11-03/crosstalk-external-sender-identity.md` and `crosstalk-toolout-evidence/`

## Context

Peer messages delivered as user input look like operator messages. Codex 0.166.0 accepts named standalone tool output through `turn/start`, both idle and active, and raw `function_call_output` items through `thread/inject_items` without starting a turn. Live probes with `experimentalApi:false` confirmed both paths, including later model consumption of a no-wake append. This refines ADR 0006's Codex transport selection without changing discovery, per-request binding, or ADR 0004's trust boundary.

## Decision

All Codex deliveries carry JSON tool data under namespace `crosstalk`, name `receive_message`. The adapter (`internal/harness/codex`) serializes message identifier, original sender name, sender harness, verified/unverified flag, kind, reply reference, parts, and a fixed peer-authority notice from the receiver-validated message. Body text cannot set these outer fields. No peer input enters operator, developer, or system instructions. Identity remains transport-validated data, not a grant of authority.

Before any delivery request the adapter checks the app-server version (tested major or refuse, `version.go`) and reads the thread; an untested major or a thread with no positive direct-input evidence is `unreachable`. Verified immediate delivery uses `turn/start` with `input:[]` and `toolOutput:{name,namespace,output,retain:true}`. An idle thread starts; an active regular turn consumes the output at its next model boundary. No active-turn lookup, expected-turn identifier, model override, permission change, or user-input fallback is needed. The connection keeps `experimentalApi:false`.

`next_turn` and all unverified-sender deliveries use `thread/inject_items` with one named `function_call_output`, preserving the same tool data, on the bound endpoint. This schedules model context without starting or steering a turn. For this adapter, the legacy `next_turn` value now means deferred until the next model request: an idle thread stays idle until independently awakened, while an active turn may consume it in a later request within that turn. It is not a strict next-turn queue. A successful append is `queued`; it is an acknowledged handoff to model history, not a shim mailbox entry or evidence of consumption. No subprocess queue is used.

Any definite `activeTurnNotSteerable` error from `turn/start`, recognized by its error message text (live codex-cli 0.166.0 sends no `error.data` there), falls back once to the same no-wake append; the app-server defines only review and compact turns as not steerable today. Every other explicit refusal is `unreachable`, with no automatic retry. Failure before a request can be sent is `unreachable`. A successful immediate request is `accepted` only with a valid returned turn identifier; a successful raw append requires its empty result object. A malformed success, lost response, disconnect, or timeout after a possibly sent request is `unknown`, never retried and never followed by another delivery method. The existing journal records these outcomes; reconciliation must not infer absence from normal turn/item history, which can omit raw injected items. Missing evidence remains `unknown`, requiring an explicit later decision rather than blind resend.

The renderer contract is limited: standalone output is a `functionCallOutput` client item, but the stock terminal renderer can hide generic outputs. Raw injected items may be absent from normal client history altogether. Never impersonate the internal `codex_tui` or `codex_app` namespaces to select their “Sent by Codex” display. Phone sender-badge support is not claimed.

## Rejected alternatives

- Keep user-message steering or `codex queue` as fallback: reintroduces operator-looking peer messages and can wake idle work.
- Inject `agent_message` as the production path: explicit author fields work for model context, but this does not provide a client-visible peer message or idle scheduling by itself.
- Add a polling scheduler or receiver mailbox implementation here: unnecessary for deferred model-context delivery and conflicts with the separate receiver work. A strict next-turn guarantee would require its own scheduling contract.
- Retry after ambiguous dispatch: may duplicate a message the harness already persisted.

## Consequences

The decision lives in the Codex adapter and its app-server client only; `adapter.Adapter` and the canonical authenticate → dedupe → hop-chain → policy → inject chain are unchanged. Diagnostic steer helpers remain for capture. Tests pin the absence of peer user input, idle/active behavior, no-wake appends, review/compact fallback, endpoint pinning, and apply-then-disconnect ambiguity. A discriminating test fails against the previous adapter and passes after implementation. Live tests use a separate binary and per-thread configuration only. No stock installation is overwritten.

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (sender harness in the payload; version and thread preflight before any delivery request; package path; hop-chain restored in the chain). Decision unchanged. Status stays Provisional.
