# ADR 0010 — One reply per ask, routes close with their task, inform routes expire

- **Status:** Provisional (overnight 2026-11-04); llm-council verdict recorded below
- **Date:** 2026-11-04
- **Deciders:** session (overnight), llm-council
- **Context source:** Codex adversarial review of design 1.9, findings R4, R6, R7, R8 (design §13); §12 open item "reply-route lifetime"

## Context

An `ask` opens an A2A task on the receiver and a reply route in the receiver's shim. Today: every `reply(message_id, text)` call creates a fresh outbound message, so a retried tool call after a lost acknowledgement delivers a second answer (R4); a reply naming a continuation message id finds no task because only the first message id is indexed (R6); a deadline expiry changes the task state but leaves the route open, a late reply closes the route and can still report `accepted`, plain informs open routes that never expire, and the ADR 0009 bounce lists every open route without bound (R7); deadline suppression on the asker side scans journaled bodies for any `in_reply_to`, including policy-rejected ones (R8).

## Decision

1. One reply per inbound message. The reply obligation is keyed by the original message id. A second `reply` with the same text returns the first result (idempotent retry). A second `reply` with different text returns `already_replied`. Multiple intentional answers are plain `send_message` informs.
2. Every message id of a task (the first message and each continuation) resolves to that task; a `reply` naming any of them completes it. The association is journaled with the continuation.
3. Routes follow their task. An ask route closes when its task reaches a terminal state (completed, failed by deadline, canceled, removed). A `reply` to a terminal ask returns `ask_closed` and sends nothing. Inform routes expire one hour after opening. The bounce lists at most the ten newest open routes and refers to `receive_message` for the rest.
4. On the asker's side, a reply counts as received only when a validated reply-receipt record exists: written after admission, bound to the expected peer and the original conversation. Deadline suppression and `replied` read that record, never arbitrary bodies.

## Rejected alternatives

- Allow repeated replies and dedupe on the receiving side: the receiver cannot tell a retry from a second answer because each `reply` mints a new message id.
- Keep inform routes open indefinitely: unbounded growth of the bounce list and of restart state; an inform nobody answered for an hour is not a conversation the model will resume by name.
- Expire ask routes on a timer independent of the task: two clocks for one fact.
- Decide route lifetime per harness: the route is shim state, not harness state (ADR 0008).

## Consequences

- `reply` gains two error reasons, `already_replied` and `ask_closed`; design §8 and tests 5 and 13 change; `TestReplyRoutesThroughJournal` currently pins the opposite of rule 1 and is rewritten.
- The journal gains a reply-receipt record type (asker side) and continuation-to-task association records.
- An inform route that expired makes a later `reply` to it return `no_such_message`; the model can still `send_message` by name.
- Implementation commits carry `Provisional-ADR: 0010`.

## llm-council verdict

(pending)
