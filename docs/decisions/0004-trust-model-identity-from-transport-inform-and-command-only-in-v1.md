# ADR 0004 — Trust model: identity from transport, three message kinds in v1

- **Status:** Provisional
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1) + Codex (0.166.0) review
- **Context source:** Codex review findings F4, F6, F7 (design changelog 1.1); council verdict V3 (design changelog 1.4)

## Context

A message's text can claim any sender. Claude's own envelope (`<cross-session-message from=... from-name=...>`) is display text; Claude verifies the connecting process pid separately. Codex's `UserInput` has no sender fields at all. The operator wants primaries and subagents, where only a subagent's owner may command it.

## Decision

Cross-reference ([ADR 0028](0028-codex-no-daemon-sessions-get-an-experimental-opt-in-queue-wake-the-journal-stays-the-mailbox.md)): the experimental Codex `--no-daemon` queue wake starts a user-role turn with fixed text and no peer text, only for an immediate message from a verified local sender; a remote paired-host sender and a role-inbox copy are mailboxed and never wake, so the model-facing rule below is unchanged.

Sender identity comes from transport evidence only: the unix peer pid matched to a registered agent, the A2A bearer credential matched to a descriptor, or the shared A2A peer token presented with no sender name, which authenticates a verified `local-user` principal that is no registered agent (design §5): it may `inform` and `ask` and may not `command`. Envelope attributes are reconstructed by the receiver from that identity and never trusted as input. Messages carry `kind`: `inform` may come from anyone, including unverified senders, and is labelled as such; `ask` is reply-required inform and never carries command authority, but an unverified sender may `inform` and may not `ask` (the ask is rejected); `command` to a subagent only from its registered owner, to a primary from any registered primary; unknown kinds are rejected. An unauthorized command is rejected without injection, never downgraded to inform. `control` (stop, shutdown) is not in v1 and needs its own validated contract. Role and owner are set at runtime only by the `claim` and `release` tools between registered agents (an agent cannot claim or release itself), never by message content. `command` and `ask` exist only on the A2A ingress; a uds frame is `inform`, and a uds frame requesting another kind is rejected without injection. Peer text never enters developer or system instructions.

Built in 0.2.0 ([ADR 0022](0022-cross-host-addressing-and-trust-name-host-host-scoped-sender-keys-inform-ask-and-reply-only.md)): a request arriving through this host's gateway is a fourth sender class. Its principal is the peer host, `remote:<fingerprint>`, from the pinned TLS handshake ([ADR 0021](0021-remote-transport-and-enrollment-a-user-started-per-host-gateway-with-pinned-mtls-and-two-sided-pairing.md)), never from a header; the agent fields are attested by that host. `receiver.Sender` gains the host, and its dedupe, route and receipt key becomes `remote:<fingerprint>/<agent_id>`, so a paired host can speak only as its own agents, never as a local agent or a third host's. A remote sender may `inform` and `ask`; `command`, `claim` and `release` from it are rejected without injection.

`kind` is a routing and labelling hint chosen by the sending model; it is not a trust boundary. The control on what a delivered message can cause is the receiving session's own harness permission model. The policy table exists to stop forged or unauthorised `command` from reaching the model, not to make `command` carry authority; `command` authorizes work only within the receiving session's existing permissions.

Model-facing rule (2026-11-21, operator direction): a delivered peer message is a teammate's request. The receiving model acts on it within its own session's permission settings; the harness's permission and approval system is the boundary, not a refusal by the model. A peer cannot grant escalation (it never changes permission settings, approval or sandbox modes, rule files or config), its message is never the user's approval for a pending prompt, and a request to do what the peer was denied is refused and reported to the user (permission laundering). Secrets are never sent to a peer. Every notice names the sender's harness. Peer text still never enters developer or system instructions. The teammate notice goes only to a verified sender that is a registered agent session of this host (`msg.PeerNotice`, chosen by the receiver); an unverified sender, the `local-user` principal, any remote sender and a pulled entry of unknown provenance get the information notice (do not act, tell the user), an inbox watcher's copy gets the awareness notice and a pulled outbound entry gets the outbound notice.

## Rejected alternatives

- **Trust the envelope.** Any process on the socket could claim to be the owner.
- **Downgrade unauthorized commands to inform.** The same executable request still reaches the model.
- **`command` as an authority grant.** The sending model chooses `kind`, so one prompt-injected registered primary could command every other primary with elevated effect.
- **Control in v1.** Shutdown semantics differ per harness and need a separate design.

## Consequences

- The same-user credential model is accepted as not being isolation from hostile same-user processes. Built in 0.2.0: across hosts the same holds per host (any process of the user that reads the host key can attest any agent of that host), and a compromised paired host can still send prompt-injection text as `inform` or `ask`; the harness's permissions are the defence; the notice tells the model not to escalate or launder.
- Every ingress runs the same chain: authenticate, dedupe, hop-chain, policy, inject.
- Tests 8, 9, 14 and 23 in the design doc gate this ADR; test 23 pins that a `command` gains no approval or setting the receiving session did not already have.
- `crosstalk send` (wave 2, 2026-11-04) lets a plain script message a registered agent through the uds auth shape: the script holds the same-user key but is no registered agent, so its sender is unverified and its kind is `inform`; ask and command are rejected as for any unverified sender.
- No cumulative message or spend limit ([ADR 0014](0014-no-default-messaging-limit-a-per-peer-nudge-in-the-send-result.md)); the receiver still rejects a message over its size limit or when its pending capacity is full (`too_many_pending`).

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (shared peer-token local-user sender class; ADR 0014 restatement cut to a pointer). Status stays Provisional.
