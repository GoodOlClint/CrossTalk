# ADR 0005 — crosstalk mcp is a stateless forwarder; crosstalk run owns the shim

- **Status:** Superseded by [ADR 0006](0006-no-spawning-the-mcp-server-is-the-shim-and-binds-from-harness-provided-identity.md)
- **Date:** 2026-11-03
- **Deciders:** operator + Claude session (council-reviewed, Codex concurrence)
- **Context source:** design.md §9, Codex council C2 and final-read CQ5–CQ7

## Context

The model reaches CrossTalk through an MCP server on stdio. The harness starts that process; for Codex the shared app-server daemon starts it, not the terminal session. The shim must hold state that outlives any one MCP connection (journal, name lock, descriptor, A2A listener, reply routing), must know which harness session it is bound to (Claude pid, Codex thread id), and must carry a name, role and owner that the model cannot change (ADR 0004). An MCP process started by the harness has none of that: its working directory and parent are the harness's or the daemon's, its lifetime is one tool connection, and its arguments are whatever the MCP configuration says.

## Decision

Two processes with one job each.

`crosstalk run --name --role [--owner] -- <harness cmd>` is the shim. It takes the name lock, replays the journal, binds its sockets, spawns (or adopts) the harness child with a per-session MCP configuration, verifies the harness binding, then publishes its descriptor.

`crosstalk mcp --shim <instance socket>` is a stateless forwarder. It serves MCP on stdio and relays every tool call to the shim's private socket. It never spawns a shim, never finds one by agent name, and exits when stdio closes. Started without `--shim`, or unable to connect, it reports the error to the model and does nothing else.

The first MCP load does not create the shim. The shim exists before the harness does.

## Rejected alternatives

- **The MCP server is the shim, spawned lazily on first load.** Attractive for installation (plain `claude` with one MCP entry, no wrapper) and on Claude the forwarder's parent pid is the session, so binding would be free. Rejected for v1 because Codex breaks it: the daemon spawns MCP servers, so the parent is the daemon and the thread id is not derivable; two sessions in one cwd would race to be "the" shim; the shim would die with the MCP connection (compaction, tool reconnect) and lose the name lock and in-flight reply routing; and name/role/owner would come from a shared MCP config that every session in that cwd reads, so two sessions could not hold distinct identities. Recorded in design.md §12 as a v1.1 candidate for the Claude adapter only, if `crosstalk run` proves too heavy for everyday use.
- **Hooks (SessionStart) create the shim.** Same identity and lifetime problems, plus the hook idempotency issue (council V9). v1.1.
- **Environment variable only (no per-session MCP config).** Rejected (CQ5): the Codex daemon may not pass the terminal child's environment to the MCP process it launches, and an inherited path cannot distinguish a fork from its parent.
- **Forwarder reconnects by agent name when its socket is gone.** Rejected (CQ6): it could attach a session to a replaced shim bound to a different conversation.

## Consequences

- Installation is `crosstalk run -- claude …` or `crosstalk run -- codex …` (herdr `agent start` wraps it). A harness started without the wrapper has no CrossTalk identity and its `crosstalk mcp`, if configured, reports `shim unavailable`.
- Binding-dependent tools return `not_ready` until the shim verifies its harness binding; a fork or a different conversation behind the same forwarder gets `unbound`.
- A shim crash leaves the harness child running; the next `crosstalk run` under that name adopts it. A shim restart therefore does not restart the model's session.
- Spike 8 must prove the per-session MCP configuration reaches the right shim on a shared Codex daemon before the Codex adapter is written.
- Gated by test 26.
