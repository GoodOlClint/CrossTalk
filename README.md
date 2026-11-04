# CrossTalk

Cross-harness agent messaging for coding-agent sessions. One small process per session lets Claude Code and Codex sessions (more harnesses later) message each other by name, with delivery into the running session through each harness's native inbound channel: Claude Code's per-session unix socket and Codex's app-server (`turn/start.toolOutput`, `thread/inject_items`). No screen scraping, no spawn-per-request, no central router.

Status: v1 is implemented; `docs/design.md` is still a draft under operator review. Install: `docs/setup.md`.

## Why

Every existing bridge either spawns a fresh `claude -p` / `codex exec` per request, polls a mailbox, or types into the terminal. Both harnesses already have a real inbound path into a live session; CrossTalk uses them and adds the parts neither harness has: discovery across harnesses, sender identity from transport evidence, a shared MCP tool surface, and an A2A-shaped message model.

## Layout

- `docs/design.md`: the design document (components, registry, message schema, ingress contracts, injector per harness, trust model, tool surface, gating tests).
- `docs/decisions/`: architecture decision records.
- `docs/setup.md`: install on one Mac.
- `plugins/crosstalk/`: the Claude Code and Codex plugin and its skill.

## License

MIT. See `LICENSE`.
