# ADR 0001 — Per-session MCP server is the messaging unit

- **Status:** Proposed
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1) + Codex (0.166.0) review
- **Context source:** design session 2026-11-03 (notes: 2026-11-03-agent-messaging-concept.md, spikes 1–4)

## Context

Coding-agent sessions (Claude Code, Codex) running in terminal panes need to message each other without a human typing and without screen scraping. Every existing project surveyed (herdr-a2a, real-a2a, a2a-bridge, ACP adapters, agent-mesh, mcp_agent_mail) either spawns a fresh process per request, polls, or types into the terminal. Both harnesses turned out to have a native inbound channel into a live TUI session: Claude Code's per-session unix socket and Codex's app-server `turn/steer` on the shared daemon. Both were verified live on 2026-11-03, including a Claude→Codex→Claude round trip using only native primitives.

## Decision

The unit of the system is one process per agent session, the **shim**. It is at once the MCP server its own model talks to (`list_agents`, `send_message`, `reply`, `ask`, `receive_message`), an A2A server and client for other shims, a Claude-compatible unix-socket ingress, and the injector into its own harness. One package, one adapter per harness (`register`, `inject`, `status`). There is no central router; messages go shim to shim. Tasks are a state machine layered on messages (A2A-shaped: contextId, taskId, parts), not a separate system.

## Rejected alternatives

- **Central gateway that routes messages** (herdr-a2a's shape). Adds a process in every path and still needs a per-harness injector; the only thing it centralises is the registry, which ADR 0003 covers without a daemon.
- **Native harness plugin without a process** (hooks only). Hooks fire at turn boundaries; neither harness lets a hook wake an idle session, and Codex has no asyncRewake equivalent.
- **Spawn-per-request A2A wrappers** (`claude -p`, `codex exec`). Stateless workers; the session in the pane is never the one that receives the message.
- **Terminal injection as the primary path** (herdr send-keys). Works, but blind to dialogs and turn state; kept only as the last rung of the delivery ladder.

## Consequences

- Every supported harness needs exactly three adapter functions and a verified native inbound path before it is added.
- The shim must be the only writer for its agent's identity (ADR 0004).
- herdr is reduced to pane host and restore; herdr-a2a is superseded.
- Spawning agents is out of scope for this repo; a separate delegate tool composes "spawn via herdr, then send_message".
