# Architecture Decision Records

One file per decision: `NNNN-title.md` (e.g. `0001-per-session-mcp-server-is-the-messaging-unit.md`).

Every ADR in this repo is provisional. An ADR is edited in place when the design or the code moves, so it states the current position; the Rejected alternatives and Context sections stay as history. An ADR replaced by another decision is marked `Superseded by ADR NNNN` (today only ADR 0005, replaced by ADR 0006 and kept as a short historical stub).

Each ADR should answer:
- **Context** — what prompted the decision
- **Decision** — what was chosen
- **Rejected alternatives** — what was considered and why not
- **Consequences** — what this commits the project to

`AGENTS.md` should reference relevant ADRs from the "Canonical pipelines" or "Architecture" sections so they enter context at session start.
