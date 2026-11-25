# ADR 0037 — Pi delivery is a custom session message, and raw peer text comes only from a verified local agent

- **Status:** Provisional (every ADR in this repo is provisional by operator ruling, 2026-11-04)
- **Date:** 2026-11-24
- **Deciders:** operator + Claude (Opus 5.5), Codex critique
- **Context source:** [change plan](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/plans/pi-harness.md) D5 to D7 and section 5; ADR 0004, ADR 0007, ADR 0028, ADR 0033, ADR 0036; Pi 1.6.0 `docs/security.md` and `dist/core/messages.js`

## Context

Pi gives an extension two ways to put text into a session. `pi.sendUserMessage` adds a message "as if the user typed it". `pi.sendMessage({customType, content, display}, {deliverAs, triggerTurn})` adds a custom message that the TUI shows under its `customType` label (measured: `[crosstalk]` above the text) and that Pi converts to a plain user-role message for the model (`messages.js`, `case "custom"`). Neither is weaker than user role. `deliverAs: "steer"` joins a running turn before the next model call; `triggerTurn: true` starts a turn on an idle session; `triggerTurn: false` on an idle session stores and shows the message without a turn (measured), and while a turn runs Pi flushes it at that run's next `turn_end`, after the whole tool batch of the turn, so it reaches the model at a turn boundary if the run continues and wakes nothing (measured, spike 24 M3).

Pi asks for no approval before tool calls (its security doc: "it does not ask for approval before every tool call"). ADR 0004 names the receiving harness's permission system as the boundary; on Pi there is none, so the model's own judgment, guided by the notice, is all that stands between a peer's text and the shell.

## Decision

1. Delivery is `pi.sendMessage` with `customType: "crosstalk"` and `display: true`, called by the extension with text the shim rendered. Never `sendUserMessage` (it hides that the text is a peer's), never a system-prompt hook (`before_agent_start`, `context_with_system`), never `expandPromptTemplates` (a peer must not run a slash command).
2. `immediate` maps to `deliverAs: "steer"`, `triggerTurn: true` (wakes an idle session: a paid model turn). `next_turn` maps to `deliverAs: "steer"`, `triggerTurn: false` (stored and shown without a turn when idle, flushed at the running run's next turn boundary when busy). `deliverAs: "nextTurn"` is not used: it is invisible until a later run incorporates it, and it lives in memory only, so a quit or `/new` before then loses it (measured). A kind does not change the mapping.
3. The rendered text is the OpenCode shape (ADR 0033): one fixed preamble line plus one JSON object whose outer fields the receiver sets (`notice`, `from`, `from_harness`, `from_agent_id`, `sender_verified`, `message_id`, `in_reply_to`, `kind`, `inbox`, `parts`). `msg.PeerNotice` learns the name "Pi".
4. Raw peer text reaches the model only from a verified local registered agent session (operator ruling, 2026-11-24, plan Q1, the same as OpenCode's ADR 0033 decision 7). A remote sender, a script sender (`local-user`) and an inbox copy are mailboxed: `Inject` returns `mailboxed`, and `MailSignal` sends the fixed text "A CrossTalk message is waiting: call receive_message" with `steer` and `triggerTurn: true`; the body reaches the model as a `receive_message` tool result with the information notice (the copy notice for an inbox copy). The fixed wake starts a turn on an idle session whatever the delivery mode, `next_turn` included.
5. Each delivery is one frame `{id, deliver, trigger, text}` on the ADR 0036 connection. `pi.sendMessage` returns before Pi admits the message, and an extension cannot see a later rejection (Pi hands it to its own listeners only; measured), so the timeout is the only failure signal. The extension sends `details: {crosstalk_id}` with the message and answers `{id, ok}` only on evidence: for `triggerTurn: true` a message event carrying that id; for an idle `next_turn` (Pi sends no event for `steer` + `triggerTurn: false`) a `custom_message` entry with that id in the session manager's in-memory entries, checked after every `next_turn` send whatever `isIdle()` says (during compaction Pi is not idle yet appends the entry at once); with no such evidence it does not answer. State mapping follows ADR 0007: ok is `accepted` (`immediate`) or `queued` (`next_turn`); no live connection before the write is `unreachable`; `ok: false` after the write, no admission within 10 s, a dropped connection or a malformed answer is `unknown`, and the frame is never re-sent. An admission that arrives after the 10 s answer (a busy `immediate` was admitted 21 s after the send) changes no reported state and sends nothing. An ok means Pi admitted the message, not that the model answered: with no model the fixed wake was admitted and the run then failed at the provider.

## Rejected alternatives

- **`pi.sendUserMessage`.** Same model position, but the TUI shows it as the operator's own typing, so the operator cannot tell a peer's words from theirs.
- **`deliverAs: "nextTurn"` for `next_turn`.** Hidden from the operator until a later run incorporates it (measured); the operator should see what the model will see.
- **Mailbox for every sender class (no raw text at all).** Offered to the operator as the stricter setting given Pi's lack of approvals; not chosen (plan section 10, Q1).
- **A tool-result delivery.** Pi has no API that puts text in as a tool result outside a tool call.

## Consequences

- An `immediate` message to an idle Pi session starts a paid model turn; setup docs say so, as for OpenCode.
- A `next_turn` or `immediate` message to a busy Pi session arrives at the next turn boundary of the running run, after the running tool batch, not at once; setup docs say so.
- A `next_turn` sent while a run streams usually reports `unknown` to the sender although Pi delivers it.
- On Pi a teammate's raw text reaches a model that can run any command without approval. The notice is the model-facing marker; M1 measures how one model treats it, and that is behavioural evidence, not a boundary.
- The script-sender and remote classes cost one extra tool call per message.

## Trial record

- Council: none sat; one-shot Codex critique recorded in the plan's "Review disposition" section.
- Rests on: ADR 0004, ADR 0007, ADR 0028, ADR 0033, ADR 0036.
- Commits: none yet (plan stage).
- Pinning test: planned in the plan's slices 2 and 4 (no `sendUserMessage`, no prompt-template expansion, hostile body keeps outer fields, state table, mailboxed classes send only the fixed text).
