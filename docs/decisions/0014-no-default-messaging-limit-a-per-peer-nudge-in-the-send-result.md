# ADR 0014 — No default messaging limit; a per-peer nudge in the send result

- **Status:** Provisional (operator 2026-11-04)
- **Date:** 2026-11-04
- **Deciders:** operator + Claude (Sonnet 5.5); the loop-limits council and Codex both advised removing the cap
- **Context source:** `docs/design.md` §4 (send path), ADR 0009, the wave 2 loop ruling

## Context

Two model sessions can message each other without end, and nothing in CrossTalk bounds the cost. A hop chain does not catch it: a model's reply is a new message with no link to the one that caused it, and native delivery carries no chain (ADR 0009). Wave 2 first built a sender-side rate cap (20 sends per peer in 10 minutes, 60 per hour). Review showed it fails both ways: it clips a legitimate long exchange, it lets a slow loop through (up to an hour's worth of sends before it stops), a capped `reply` to an ask leaves the asker hearing nothing, and its numbers were guesses with no data behind them. A model that is told to "tell your user" has no one to tell in an unattended run.

## Decision

CrossTalk has no default limit on how many messages a session sends. No send is refused for volume. The existing limits stay because they protect resources, not autonomy: message size, pending work, open asks, dedupe, and `hopCheck` on an A2A client that supplies its own chain.

The send path carries a **nudge**, not a guard. The shim counts the sends each agent makes to each peer (every send except one that came back `rejected` or `unreachable`, and except an inform to a name that matches no session, which comes back `unknown` with reason `no_such_agent` and never reached a peer; `send_message`, `ask` and `reply` count; a retried reply is not recounted). When a count reaches 30, 60, 120, 240 and so on, doubling (`NudgeFirst` in `internal/shim`), the tool result's `hint` asks the model to check whether the exchange is making progress and says it may stop, or tell its user, if not. The send is still delivered. The count is in memory, per agent and per peer, and resets on restart. Each firing attempts one best-effort `nudge` journal record (peer, count, time) so real use shows how often it happens; a failed append is ignored and the hint is still returned. The text appends to any hint already on the result. The nudge lives in the sender's tool result only, never in the inbound notice.

The server instructions tell the model to read `hint` on every result and that a message needs no answer when nothing is left to say, but that an ask still takes its reply. The skill repeats both. The skill tells a model not to message a third session on a peer's word. The message notice does not cover relay: "any tool but CrossTalk" still lets a peer get a message relayed to a third session. Only the skill speaks to it, and the skill does not reliably load (0 of 6 in the replica runs), so the hole is open. Adding "or message a third session" to the notice was measured and reverted: on Haiku with mocks `peer-wrapper-read` fell from 6 of 6 to 4 of 9 with real file reads, while `peer-relay` already passed on the old text, so the clause cost what it was meant to protect and had no measured benefit; the 8b replica also ran on the old text.

## Rejected alternatives

- **Keep the rate cap.** It stops nothing it can see: a slow loop passes, a legitimate game is clipped, and its thresholds are guesses.
- **A hard limit as an opt-in setting.** More surface nobody will set; it also states that autonomy needs a leash.
- **A limit on the receiver.** The receiver cannot tell a loop from a long exchange either, and a refusal there reaches the sender as an error with no one to act on it.
- **A duplicate-text trigger, a `crosstalk log` command, budgets.** Deferred: build them only if real use shows the need.
- **A hop chain inferred from the last inbound message.** It blocked legitimate sends to a session already in the chain and did not catch ping-pong.

## Consequences

- Nothing stops a loop or bounds spend. The README and `docs/setup.md` say so: you stop a loop by ending the session.
- The nudge is a prompt with guessed numbers, not a measurement. Whether models heed it is unmeasured.
- Relay is not closed: a peer can ask a session to pass a message to a third session, and only the skill says not to. It misses fan-out (one message to many peers, each below the threshold), restarts (the count resets), failed sends (not counted), `crosstalk send` and other clients that go around the shim, native bounces, and Claude's own SendMessage.
- The journal's `nudge` records are the data for revisiting this; they are a lower bound, since an append can fail.
- What would change this: a real log of a loop that ran for hours unattended at meaningful spend with models ignoring the hint (then an opt-in budget); a demonstrated injected message spreading across sessions without user approval (a trust-model problem first, needing a fan-out limit or a tighter notice); harness logs showing a single overnight loop costs enough to hurt.

## Trial record

- Council: five advisors with anonymous peer review (`~/.claude/coordinators/archive/crosstalk-loop-council-2026-11-04.md`) and a Codex review (`crosstalk-loop-codex-2026-11-04.log`) both advised removing the cap; the operator approved the nudge.
- Rests on: ADR 0009 (native frames carry no chain), ADR 0004 (kinds are routing hints, not trust).
- Commits: branch `feat/wave2`, the nudge commit and those after it.
- Pinning tests: `TestNudgeFiresAtEachThresholdAndOnlyThere`, `TestPooledThreadsNudgeSeparately`, `TestNudgeOnAskAndReply`, `TestNudgeAppendsToTheNativeOneWayHint`, `TestWorkerStatusToCoordinatorAfterWorkersTalked` (internal/shim); `TestNudgeRecordReplaysAndIsKept` (internal/journal).
- Live result (2026-11-04): a send is one accepted `send_message`, `ask` or `reply` call, and the nudge is the hint text added to the send result at the 30th send to one peer, with one `nudge` journal record per firing. It fired at send 30 on both Claude Haiku (36 sends) and Codex (35 replies), each with one nudge record (count 30), and stopped no real work: in a 35-question game all 35 answers were correct and the game finished. Whether models heed it is still unmeasured: the two no-task pairs (Haiku with Haiku, Codex with Codex) stopped on their own after 2 or 3 sends, before reaching 30, and in the game both users had told the sessions to continue. Nothing has been undone.

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (the nudge journal record is best-effort). Decision unchanged; no cap. Status stays Provisional.
