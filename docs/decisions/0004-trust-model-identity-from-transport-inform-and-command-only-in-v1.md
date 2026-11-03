# ADR 0004 — Trust model: identity from transport, inform and command only in v1

- **Status:** Proposed
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1) + Codex (0.166.0) review
- **Context source:** Codex review findings F4, F6, F7 (design changelog 1.1)

## Context

A message's text can claim any sender. Claude's own envelope (`<cross-session-message from=... from-name=...>`) is display text; Claude verifies the connecting process pid separately. Codex's `UserInput` has no sender fields at all. The operator wants primaries and subagents, where only a subagent's owner may command it.

## Decision

Sender identity comes from transport evidence only: the unix peer pid matched to a registered agent, or the A2A bearer credential matched to a descriptor. Envelope attributes are reconstructed by the receiver from that identity and never trusted as input. Messages carry `kind`: `inform` may come from anyone, including unverified senders, and is labelled as such; `command` to a subagent only from its registered owner, to a primary from any registered primary; unknown kinds are rejected. An unauthorized command is rejected without injection, never downgraded to inform. `control` (stop, shutdown) is not in v1 and needs its own validated contract. Role and owner are fixed at shim start and not changeable by the model. Peer text never enters developer or system instructions.

## Rejected alternatives

- **Trust the envelope.** Any process on the socket could claim to be the owner.
- **Downgrade unauthorized commands to inform.** The same executable request still reaches the model.
- **Control in v1.** Shutdown semantics differ per harness and need a separate design.

## Consequences

- The same-user credential model is accepted as not being isolation from hostile same-user processes.
- Every ingress runs the same chain: authenticate, dedupe, hop-chain, policy, inject.
- Tests 8, 9 and 14 in the design doc gate this ADR.
