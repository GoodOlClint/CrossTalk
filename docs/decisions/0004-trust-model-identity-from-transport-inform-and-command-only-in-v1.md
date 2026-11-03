# ADR 0004 — Trust model: identity from transport, inform and command only in v1

- **Status:** Proposed
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1) + Codex (0.166.0) review
- **Context source:** Codex review findings F4, F6, F7 (design changelog 1.1); council verdict V3 (design changelog 1.4)

## Context

A message's text can claim any sender. Claude's own envelope (`<cross-session-message from=... from-name=...>`) is display text; Claude verifies the connecting process pid separately. Codex's `UserInput` has no sender fields at all. The operator wants primaries and subagents, where only a subagent's owner may command it.

## Decision

Sender identity comes from transport evidence only: the unix peer pid matched to a registered agent, or the A2A bearer credential matched to a descriptor. Envelope attributes are reconstructed by the receiver from that identity and never trusted as input. Messages carry `kind`: `inform` may come from anyone, including unverified senders, and is labelled as such; `ask` is reply-required inform with inform's permissions and never carries command authority; `command` to a subagent only from its registered owner, to a primary from any registered primary; unknown kinds are rejected. An unauthorized command is rejected without injection, never downgraded to inform. `control` (stop, shutdown) is not in v1 and needs its own validated contract. Role and owner are fixed at shim start and not changeable by the model. Peer text never enters developer or system instructions.

`kind` is a routing and labelling hint chosen by the sending model; it is not a trust boundary. The control on what a delivered message can cause is the receiving session's own harness permission model. The policy table exists to stop forged or unauthorised `command` from reaching the model, not to make `command` carry authority; `command` authorizes work only within the receiving session's existing permissions.

## Rejected alternatives

- **Trust the envelope.** Any process on the socket could claim to be the owner.
- **Downgrade unauthorized commands to inform.** The same executable request still reaches the model.
- **`command` as an authority grant.** The sending model chooses `kind`, so one prompt-injected registered primary could command every other primary with elevated effect.
- **Control in v1.** Shutdown semantics differ per harness and need a separate design.

## Consequences

- The same-user credential model is accepted as not being isolation from hostile same-user processes.
- Every ingress runs the same chain: authenticate, dedupe, hop-chain, policy, inject.
- Tests 8, 9, 14 and 23 in the design doc gate this ADR; test 23 pins that a `command` gains no approval or setting the receiving session did not already have.
