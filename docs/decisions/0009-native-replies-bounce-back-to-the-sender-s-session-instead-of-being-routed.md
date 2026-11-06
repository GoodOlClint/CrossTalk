# ADR 0009 — Native replies bounce back to the sender's session instead of being routed

- **Status:** Provisional
- **Date:** 2026-11-04
- **Deciders:** operator + Claude
- **Context source:** live round 4 on 7bf18d1 (docs/spikes/10-live-2026-11-03.md, defect 6); operator ruling 2026-11-04

## Context

Claude's native `SendMessage` to the `uds:` address in an inbound envelope reaches the receiving session's own shim socket as a single unauthenticated line with no message id. Design §5 (F4, F6) routed that frame to the original sender when exactly one reply route into the session was open, and until e6a3b2b also completed the matching ask. Round 4 showed a native acknowledgement of an inform, sent after the inform's route was closed by the `reply` tool, matching the sole remaining route (an ask) and completing it with the inform's text. e6a3b2b made the guess non-terminal, but the shim still forwards text to a sender it only guessed, with an `in_reply_to` hint that can be wrong, and the model can answer twice (natively and with the tool).

The shim never has the context to correlate a native frame. The model that sent it does.

## Decision

A native frame from the bound session is never forwarded. The shim journals it as `bounced` and injects a short message back into the same session (through the harness `Inject`, from `crosstalk`) saying the native message was not delivered and listing the open inbound messages it may answer (every open ask, then the ten newest informs and a count of the rest, ADR 0010), each as `message_id`, sender name and kind, to answer with `reply`. With no open route it says so and names `send_message`. The bounce quotes the first line of the undelivered text, cut to 200 characters, so the model can repeat it.

A bounce is an injection, not a delivery: it opens no route, is never bounced itself, and completes nothing. Only `reply` completes an ask (e6a3b2b). Routes close by `reply`, by their ask task reaching a terminal state, by a rejected or unreachable injection, or by inform expiry (ADR 0010); never by a native frame. Delivered messages therefore always carry an authoritative `in_reply_to` or none.

The `uncorrelated` state is replaced by `bounced` (mailbox only). The receiver's `Forwarder` and the sole-open-route lookup are removed.

## Rejected alternatives

- Route by sole open route (F4/F6, drafts 1.1–1.8): guesses; produced defect 6; even non-terminal it delivers text to a guessed recipient with a possibly wrong hint.
- Route by sole open route and ask the sender to confirm: two extra turns and still a guess.
- Parse the native text for a message id: the model rarely includes one, and text is not a trust input (§5 C8).
- Drop native frames silently as `uncorrelated` only: the sending model believes its message was delivered (Claude reports success on EOF) and the conversation stalls.

## Consequences

- Design §5 native-reply paragraph, §8 states and §11 test 5 rewritten; route lifetime is ADR 0010.
- One extra model turn in the sending session per native attempt; the text is lost unless the model repeats it.
- A model that answers a bounce natively gets another bounce; each needs a model turn, so this is bounded by the model, not by the shim. The bounce text says not to reply to it natively.
- Codex is unaffected (no native path; `NativeReplyAddress` is empty). A future harness with a native reply path gets this behaviour for free.
- Supersedes the native-routing part of design F4/F6; ADR 0004 (trust model) unchanged.
- Native frames carry no hop chain and no message limit applies (ADR 0014); a model answering a native bounce natively is outside the per-peer nudge.

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (route-close paths named, with route lifetime pointed at ADR 0010; bounce wording and the 200-character cap; wave 2 bullet cut to what ADR 0014 does not already say). Decision unchanged; kept separate from 0010. Status stays Provisional.
