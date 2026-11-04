# ADR 0009 — Native replies bounce back to the sender's session instead of being routed

- **Status:** Accepted (provisional, operator 2026-11-04)
- **Date:** 2026-11-04
- **Deciders:** operator + Claude
- **Context source:** live round 4 on 7bf18d1 (docs/spikes/10-live-2026-11-03.md, defect 6); operator ruling 2026-11-04

## Context

Claude's native `SendMessage` to the `uds:` address in an inbound envelope reaches the receiving session's own shim socket as a single unauthenticated line with no message id. Design §5 (F4, F6) routed that frame to the original sender when exactly one reply route into the session was open, and until e6a3b2b also completed the matching ask. Round 4 showed a native acknowledgement of an inform, sent after the inform's route was closed by the `reply` tool, matching the sole remaining route (an ask) and completing it with the inform's text. e6a3b2b made the guess non-terminal, but the shim still forwards text to a sender it only guessed, with an `in_reply_to` hint that can be wrong, and the model can answer twice (natively and with the tool).

The shim never has the context to correlate a native frame. The model that sent it does.

## Decision

A native frame from the bound session is never forwarded. The shim journals it as `bounced` and injects a short message back into the same session (through the harness `Inject`, from `crosstalk`) saying the native message was not delivered and listing the open inbound messages it may answer, each as `message_id`, sender name and kind, with the instruction to use `reply` (for an inbound message) or `send_message` (for anything else). With no open route it says so and names `send_message`. The bounce quotes the first line of the undelivered text so the model can repeat it.

A bounce is an injection, not a delivery: it opens no route, is never bounced itself, and completes nothing. `reply` remains the only path that completes an ask or closes a route (e6a3b2b). Delivered messages therefore always carry an authoritative `in_reply_to` or none.

The `uncorrelated` state is replaced by `bounced` (mailbox only). The receiver's `Forwarder` and the sole-open-route lookup are removed.

## Rejected alternatives

- Route by sole open route (F4/F6, drafts 1.1–1.8): guesses; produced defect 6; even non-terminal it delivers text to a guessed recipient with a possibly wrong hint.
- Route by sole open route and ask the sender to confirm: two extra turns and still a guess.
- Parse the native text for a message id: the model rarely includes one, and text is not a trust input (§5 C8).
- Drop native frames silently as `uncorrelated` only: the sending model believes its message was delivered (Claude reports success on EOF) and the conversation stalls.

## Consequences

- Design §5 native-reply paragraph, §8 states and §11 test 5 rewritten; §12 "reply-route lifetime" item stays (routes are closed only by `reply` or deadline).
- One extra model turn in the sending session per native attempt; the text is lost unless the model repeats it.
- A model that answers a bounce natively gets another bounce; each needs a model turn, so this is bounded by the model, not by the shim. The bounce text says not to reply to it natively.
- Codex is unaffected (no native path; `NativeReplyAddress` is empty). A future harness with a native reply path gets this behaviour for free.
- Supersedes the native-routing part of design F4/F6; ADR 0004 (trust model) unchanged.
