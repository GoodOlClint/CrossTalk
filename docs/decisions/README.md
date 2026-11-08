# Architecture Decision Records

One file per decision: `NNNN-title.md` (e.g. `0001-per-session-mcp-server-is-the-messaging-unit.md`).

Every ADR in this repo is provisional. An ADR is edited in place when the design or the code moves, so it states the current position; the Rejected alternatives and Context sections stay as history. An ADR replaced by another decision is marked `Superseded by ADR NNNN` (today only ADR 0005, replaced by ADR 0006 and kept as a short historical stub).

Built in main, arrives in 0.2.0 and is not yet released: [ADR 0021](0021-remote-transport-and-enrollment-a-user-started-per-host-gateway-with-pinned-mtls-and-two-sided-pairing.md) (remote transport and enrollment) and [ADR 0022](0022-cross-host-addressing-and-trust-name-host-host-scoped-sender-keys-inform-ask-and-reply-only.md) (cross-host addressing and trust) record the v0.2.0 remote design. The paragraphs marked "Built in main, arrives in 0.2.0 and is not yet released" in ADRs 0001, 0003, 0004, 0006 and 0011 describe the same design as built; the live two-machine run has not been run.

Each ADR should answer:
- **Context** — what prompted the decision
- **Decision** — what was chosen
- **Rejected alternatives** — what was considered and why not
- **Consequences** — what this commits the project to

`AGENTS.md` should reference relevant ADRs from the "Canonical pipelines" or "Architecture" sections so they enter context at session start.
