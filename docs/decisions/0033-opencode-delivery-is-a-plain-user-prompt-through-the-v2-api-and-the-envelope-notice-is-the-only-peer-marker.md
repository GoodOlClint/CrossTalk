# ADR 0033 — OpenCode delivery is a plain user prompt through the v2 API, and the envelope notice is the only peer marker

- **Status:** Provisional (every ADR in this repo is provisional by operator ruling, 2026-11-04)
- **Date:** 2026-11-23
- **Deciders:** operator + Claude (Sonnet 5.5), Codex critique
- **Context source:** [spike 21](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/spikes/21-opencode.md) Q1, Q4, Q5; [change plan](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/plans/opencode-harness.md) sections 3 and 5; ADR 0004, ADR 0007

## Context

No OpenCode API delivers a message as a tool result or a role weaker than user. `POST /api/session/<id>/prompt` (v2) puts the text in as a user message, mid-turn at the next model call (`steer`) or after the turn (`queue`). Other routes exist and are hazards: `synthetic` text reaches the model as user input but is hidden from the operator; v1's `system` field and v2's instructions endpoint write into system instructions; the `/tui/*` routes edit the operator's own prompt box and merge with a half-typed draft. Issue #273 named `/tui/append-prompt` as an option; the spike found it unsafe. ADR 0004 requires that peer text never enters developer or system instructions and that the notice and the harness's permission system, not the message position, are the control.

## Decision

1. The OpenCode adapter delivers with `POST <service url>/api/session/<id>/prompt`, Basic auth `opencode:<password from service.json>`, one plain text body. It never sets `synthetic`, `system` or any instruction field, never calls the v2 instructions endpoint, never calls a `/tui/` path, and never uses a plugin system transform. A test records every request to a fake service and fails on any of these.
2. `immediate` maps to `delivery: steer`; `next_turn` maps to `delivery: queue` with `resume: false`, which the pre-build measurement showed stores the message without starting a turn (spike 22 Q3; a plain `queue` on an idle session starts one, Q2); `Inject` reports it `Queued`. `immediate` sends no `resume` field. A kind does not change the mapping.
3. The rendered text is a fixed preamble line plus one JSON object whose outer fields (`notice`, sender, harness, verified flag, message id, reply reference, kind, inbox) are set by the receiver; the peer's body is a JSON string value, so no delimiter can forge an envelope. Notice classes and their texts are unchanged from ADR 0004; `msg.PeerNotice` learns the name "OpenCode" for a sender on that harness.
4. State mapping follows ADR 0007: a 200 with an admitted message id is `accepted` (`steer`) or `queued` (`queue`); a refusal before any byte is sent (service down, 401, 404, refused major, unsafe `service.json`) is `unreachable`; a malformed success or a loss after sending is `unknown` and is never retried.
5. The password is read from `service.json` per request and never logged or stored in a binding, descriptor, journal or error. The adapter refuses a file that is not mode 0600 and owned by the current user, and a `url` whose host is not loopback.
6. The operator sees what the model sees: delivery is visible in the TUI as a prompt.
7. Raw peer text is a user prompt only for a verified local registered agent. A remote sender, a script sender and an inbox copy are mailboxed (`Inject` returns `mailboxed`, the journal holds the body) and announced by one fixed prompt with no peer words, "A CrossTalk message is waiting: call receive_message"; the body then arrives as a tool result with the information notice. This follows ADR 0007's rule that the only user-role wake carries fixed text and ADR 0028's mailbox path. The operator chose this setting on 2026-11-23 (plan section 10, Q8).
8. The package opens `service.json` with no-follow, checks mode 0600 and owner on the opened descriptor, builds one canonical loopback origin used for the binding and every request, and disables redirects on authenticated requests.

## Rejected alternatives

- **`synthetic` parts.** Hides peer text from the operator while the model reads it as user input.
- **`/tui/append-prompt` plus `/tui/submit-prompt`.** Edits the operator's draft, a collision hazard like typing into a terminal.
- **v2 instructions endpoint or v1 `system`.** Puts peer text in system instructions, forbidden by ADR 0004.
- **A new shim mailbox with hook-driven pointers and a collect transaction (the Antigravity design).** Unneeded: the service accepts a prompt at any time. Decision 7 reuses only the existing journal mailbox and `MailSignaler` for the non-local classes; it adds no store.
- **Envelope in angle-bracket text (Claude's form).** Needs escaping rules that a JSON string gives for free.

## Consequences

- A peer message is a user-role message for OpenCode, as it is for Claude and stronger than for Codex; the notice text is the model-facing marker. Whether a real model honors it: M1 ran on 2026-11-23 and was inconclusive on protection (run 4, no notice, refused too), so the follow-up is L11 and the compat live leg; if a model does not honor it, raw delivery stays off for every sender class and OpenCode gets only the fixed wake prompt and `receive_message` (decision 7's path for everyone), whatever the kind, until the notice is fixed.
- Of the direct teammate prompts, only an `immediate` one wakes an idle session and starts a paid model turn; a `next_turn` one is stored without a turn (spike 22 Q3, plan Q6) and is seen when the session next runs. A mailbox-only class (remote sender, script sender, inbox copy) is journaled, and its fixed wake (`MailSignal`) is sent as `steer` whatever the mode, so it can start a turn on an idle session. Setup docs say so.
- Any same-user process can inject with or without CrossTalk (ADR 0004 Consequences); CrossTalk adds no hole and cannot attribute a message it did not send.
- A second local user is excluded by the 0600 password files (the state and the config copy); the second-account probe (plan M2) has not been run and is deferred (no second account, operator, 2026-11-23, plan Q4), so the release cannot claim that coverage.

## Trial record

- Council: none sat; one-shot Codex critique recorded in the plan's "Codex critique" section.
- Rests on: ADR 0004, ADR 0007, ADR 0008, ADR 0032.
- Commits: none yet (plan stage).
- Pinning test: planned in the plan's slice 4 (no forbidden route or field, state table, hostile body) and slice 2 (credential handling).
