# ADR 0001 — Per-session MCP server is the messaging unit

- **Status:** Provisional
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1) + Codex (0.166.0) review
- **Context source:** design session 2026-11-03 (spikes 1–4 and the cross-harness round trip: `docs/spikes/README.md`)

## Context

Coding-agent sessions (Claude Code, Codex) running in terminal panes need to message each other without a human typing and without screen scraping. Every existing project surveyed (herdr-a2a, real-a2a, a2a-bridge, ACP adapters, agent-mesh, mcp_agent_mail) either spawns a fresh process per request, polls, or types into the terminal. Both harnesses turned out to have a native inbound channel into a live TUI session: Claude Code's per-session unix socket and Codex's app-server on the shared daemon (first verified through `turn/steer`; delivery is now `turn/start` with `toolOutput`, ADR 0007). Both were verified live on 2026-11-03, including a Claude→Codex→Claude round trip using only native primitives.

## Decision

The unit of the system is the **shim**, the `crosstalk mcp` process a harness starts for its MCP connection (ADR 0006). One shim process may serve several sessions (Codex pools connections), so the per-session unit is the agent record, one per (harness, session_ref). For each agent it serves, the shim is at once the MCP server that agent's model talks to (`list_agents`, `send_message`, `reply`, `ask`, `receive_message`, `claim`, `release`), an A2A server and client for other shims, a Claude-compatible unix-socket ingress, and the injector into its own harness. One package per harness behind `harness.Harness` (ADR 0008). There is no central router; messages go shim to shim. Tasks are a state machine layered on messages (A2A-shaped: contextId, taskId, parts), not a separate system.

Built in main, arrives in 0.2.0 and is not yet released ([ADR 0021](0021-remote-transport-and-enrollment-a-user-started-per-host-gateway-with-pinned-mtls-and-two-sided-pairing.md)): across machines, each host may run one gateway (`crosstalk remote serve`), started by the user. It is a per-host forwarder, not a central router: it authenticates the peer host and forwards each message to the target agent's own loopback A2A server, where that agent's receiver chain runs as for any message. Senders dial the target host's gateway directly; no message passes through a third host.

## Rejected alternatives

- **Central gateway that routes messages** (herdr-a2a's shape). Adds a process in every path and still needs a per-harness injector; the only thing it centralises is the registry, which ADR 0003 covers without a daemon.
- **Native harness plugin without a process** (hooks only). Hooks fire at turn boundaries; neither harness lets a hook wake an idle session, and Codex has no asyncRewake equivalent.
- **Spawn-per-request A2A wrappers** (`claude -p`, `codex exec`). Stateless workers; the session in the pane is never the one that receives the message.
- **Terminal injection as the primary path** (herdr send-keys). Works, but blind to dialogs and turn state; kept out of v1 (design §10); the herdr pane id is not recorded.

## Consequences

- Every supported harness implements `harness.Harness` (ADR 0008). Currently supported harnesses require a verified native inbound path; a separately decided and verified deferred-delivery path may qualify a future harness (planned in [ADR 0012](0012-antigravity-harness-via-hook-mailbox.md), which keeps a shim process and so is not the hooks-only design rejected above).
- Sender identity comes from transport evidence, never from message text (ADR 0004); a session's own identity comes from harness-provided evidence on each call (ADR 0006).
- The shim is the only writer of its own agent's identity: a claim or release is applied in the target agent's own process, against that agent's own journal, and never written by another shim (ADR 0004).
- An agent host (herdr, now Herdr) is reduced to pane host and restore; herdr-a2a is superseded.
- Spawning agents is out of scope for this repo; a separate tool composes "launch through an external agent host, then send_message".

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (deferred-delivery exception for ADR 0012 named as planned; harness-neutral delegate line). Status stays Provisional.
