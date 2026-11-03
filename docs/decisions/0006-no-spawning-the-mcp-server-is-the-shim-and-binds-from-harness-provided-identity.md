# ADR 0006 — No spawning: the MCP server is the shim and binds from harness-provided identity

- **Status:** Proposed
- **Date:** 2026-11-03
- **Deciders:** operator + Claude session + Codex session
- **Context source:** operator redirect 2026-11-03 ("build this without needing to spawn processes on our own; communicate with any Claude or Codex session running"); Codex source read of rust-v0.166.0 `codex-rs/core/src/mcp_tool_call.rs`; live process inspection on this machine

## Context

ADR 0005 made `crosstalk run` the process owner because an MCP server process could not tell which session it served. That forced every session to be launched through CrossTalk. Two facts remove the need. Claude Code starts MCP servers as children of the session process, so the server's parent pid names the session and `~/.claude/sessions/<ppid>.json` carries its name, socket and status. Codex attaches `params._meta.threadId` and `sessionId` to every MCP `tools/call` it issues (`with_mcp_tool_call_ids_meta`), and the app-server's own `mcpServer/tool/call` sets `_meta.threadId` too. Codex 0.166 can also pool one stdio MCP connection across threads when the 2026-07-28 protocol is negotiated, so a process is not reliably one-per-thread.

Inbound delivery already needs nothing installed on the receiver: Claude's per-session socket and Codex's app-server `turn/steer` and `codex queue` reach any running session.

## Decision

CrossTalk spawns nothing and wraps nothing. `crosstalk mcp` is the shim, installed once per harness as an ordinary MCP server entry. It serves whatever sessions the harness connects to it and binds identity per request: Claude from the parent pid; Codex from `_meta.threadId` on each `tools/call`. A request with neither is `unbound` and gets no messaging tool. One process may hold several agent records (Codex pooling); nothing is bound permanently to the first caller.

Every running session is listed by `list_agents` whether or not it has CrossTalk loaded, from `~/.claude/sessions` and `thread/loaded/list` on known app-servers, with `reachable` and `crosstalk` flags. A session without CrossTalk can receive but cannot send, has no stable name beyond its harness name, and has no A2A card.

Role and owner are asserted by the owner (`claim`), not by a launcher. `crosstalk run` is removed from v1.

## Rejected alternatives

- **ADR 0005 wrapper (`crosstalk run`).** Superseded. It required launching every session through CrossTalk, which the operator rejected; its binding problem is solved by harness-provided identity.
- **Activity inference (the single active thread is the caller).** Rejected by both sessions: concurrent active threads and status races defeat it, and an app-server tool call need not come from an active turn.
- **Nonce handshake via `mcpServer/tool/call`.** Workable but unnecessary once `_meta.threadId` is present on ordinary calls; kept as a diagnostic (`crosstalk capture`), not as the binding.
- **Environment or cwd inheritance.** Dead: Codex MCP children carry only `CODEX_HOME` and `CODEX_CLI_PATH`; cwd is the configured or fallback value.

## Consequences

- Install is one line per harness; plain `claude` and plain `codex` work. herdr needs no CrossTalk knowledge.
- The Codex `_meta` contract is release-source evidence, not a documented API: pinned to 0.166 in the version check, captured live as spike 9 before the Codex adapter is written, re-verified per Codex release.
- A pooled Codex connection means per-request agent lookup and no process-level "my session" state; the journal, descriptor and name-stable socket are per agent, not per process.
- A session whose shim process has exited (MCP connection closed) is still reachable natively but its name-stable socket has no listener until the harness reconnects the MCP server; design.md §9 states this.
- Supersedes ADR 0005.
