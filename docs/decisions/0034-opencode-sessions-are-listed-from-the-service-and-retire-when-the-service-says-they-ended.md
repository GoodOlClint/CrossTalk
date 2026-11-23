# ADR 0034 — OpenCode sessions are listed from the service and retire when the service says they ended

- **Status:** Provisional (every ADR in this repo is provisional by operator ruling, 2026-11-04)
- **Date:** 2026-11-23
- **Deciders:** operator + Claude (Sonnet 5.5), Codex critique
- **Context source:** [spike 21](../spikes/21-opencode.md) Q6; [change plan](../plans/opencode-harness.md) decisions D4, D6, D7; ADR 0003, ADR 0008

## Context

The v2 service lists every stored session (`GET /api/session`) and the running ones (`GET /api/session/active`); no endpoint says which session a TUI shows, and the service outlives every TUI. One MCP process serves every session of a project directory, so the process cannot prove a session ended. The `harness.Supersession` interface asks for proof that a session ended while its MCP connection lives on, and says one process serving several live sessions is not such proof. Without a filter an old project's sessions would all appear in `list_agents` and take names; without retirement a deleted session would keep its name until the shim exits, which for a service-hosted MCP process may be never.

## Decision

1. `Discover` lists sessions that are not archived, are top-level (no `parentID`), and were updated within 24 hours, newest 30 at most (named constants). A session CrossTalk has bound is always listed regardless of age. The row's name is the session title, cwd the session's directory, status busy or idle from `/api/session/active`. A service that is down or a missing `service.json` yields no rows and no error.
2. There is no `ClaimSource` and no polling loop or event subscription. A listed session that never called CrossTalk is an unbound row, reachable by native name through `Register` and `Inject`, and binds at its first CrossTalk tool call (the Codex pattern).
3. The package implements `Supersession`: `Superseded` is true only when the service answers 404 for the session or reports it archived. An idle session is not superseded. Every error, a down service or a missing file answers false. Answers are cached for a few seconds so the registry's 2 s sweep sends at most one request per agent per interval. The process id is never used as evidence.

## Rejected alternatives

- **List every stored session.** Fills `list_agents` and claims names for sessions nobody uses.
- **Retire on idle time.** An idle session is a normal state and a message to it works; retiring it would break reachability.
- **Subscribe to `GET /api/event`.** A long-lived connection and reconnect logic for a lookup the sweep already makes on demand; revisit only if staleness is measured to hurt.
- **Use the service pid as evidence.** One process serves all sessions; the interface rules this out.

## Consequences

- A session older than 24 hours that never called CrossTalk is not listed until it is active again; it can still be reached by a send that names its id through a bound peer, not by name. The operator may change the window (plan section 10, Q2).
- A deleted or archived session drops from `list_agents` within a sweep; a send to it is `unreachable`.
- The 24-hour and 30-session values are not measured against real use; the live run records what `list_agents` shows.
- If `GET /api/session/<id>` 404 or the archived field behave differently from the source reading, slice 3 changes; measurement M6 checks both.

## Trial record

- Council: none sat; one-shot Codex critique recorded in the plan's "Codex critique" section.
- Rests on: ADR 0003, ADR 0008, ADR 0032.
- Commits: none yet (plan stage).
- Pinning test: planned in the plan's slice 3 (filter table, `Superseded` true only on 404 or archived, false on every error).
