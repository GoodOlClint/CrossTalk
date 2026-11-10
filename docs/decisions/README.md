# Architecture Decision Records

One file per decision: `NNNN-title.md` (e.g. `0001-per-session-mcp-server-is-the-messaging-unit.md`).

Every ADR in this repo is provisional. An ADR is edited in place when the design or the code moves, so it states the current position; the Rejected alternatives and Context sections stay as history. An ADR replaced by another decision is marked `Superseded by ADR NNNN` (today only ADR 0005, replaced by ADR 0006 and kept as a short historical stub).

Built in 0.2.0: [ADR 0021](0021-remote-transport-and-enrollment-a-user-started-per-host-gateway-with-pinned-mtls-and-two-sided-pairing.md) (remote transport and enrollment) and [ADR 0022](0022-cross-host-addressing-and-trust-name-host-host-scoped-sender-keys-inform-ask-and-reply-only.md) (cross-host addressing and trust) record the v0.2.0 remote design. The paragraphs marked "Built in 0.2.0" in ADRs 0001, 0003, 0004, 0006 and 0011 describe the same design as built; run live on 2026-11-08 between a Mac and a Linux VM over a Wi-Fi LAN with a build from main (pairing, both directions, settle, the login unit through a reboot); not run over a tailnet, a wired LAN, or with the signed release binary.

Role inboxes are [ADR 0024](0024-role-inboxes-beside-session-names.md), built with `attach`, `detach`, `grant`, the `inbox:` form of `claim` and `to`, copies to watchers and membership notices; its fallback walk past an unreachable responder is a follow-up and not built. The pre-bind store is [ADR 0026](0026-pre-bind-store-messages-delivered-before-a-session-binds-are-kept-and-replayed-so-reply-finds-them.md), built: a reply to an inform delivered before the recipient bound finds its route after the bind.

Each ADR should answer:
- **Context** — what prompted the decision
- **Decision** — what was chosen
- **Rejected alternatives** — what was considered and why not
- **Consequences** — what this commits the project to

`AGENTS.md` should reference relevant ADRs from the "Canonical pipelines" or "Architecture" sections so they enter context at session start.
