# CrossTalk

Cross-harness agent messaging for coding-agent sessions. One small process per session lets Claude Code and Codex sessions (more harnesses later) message each other by name, with delivery into the running session through each harness's native inbound channel: Claude Code's per-session unix socket and Codex's app-server (`turn/start.toolOutput`, `thread/inject_items`). No screen scraping, no spawn-per-request, no central router.

Status: v1 is implemented; `docs/design.md` is still a draft under operator review. Install: download the release binary and run `./crosstalk install` (`docs/setup.md`, macOS and Linux). CI builds and tests on both; a Linux install from the release asset has not been run live yet. Nothing stops a message loop or bounds spend: two sessions can message each other without end, and you stop one by ending the session.

## Demo

Claude Code and Codex play 20 Questions over CrossTalk 0.1.2:

https://github.com/user-attachments/assets/18422ed7-2353-4aed-97fd-0a9152749fbe

Unedited. "May I" is the Codex model asking its user before acting on a peer's request, as the skill tells it to. It isn't a tool prompt. The first `ask` is refused because Codex hasn't connected to CrossTalk yet, so Claude sends a plain message instead.

## Why

Every existing bridge either spawns a fresh `claude -p` / `codex exec` per request, polls a mailbox, or types into the terminal. Both harnesses already have a real inbound path into a live session; CrossTalk uses them and adds the parts neither harness has: discovery across harnesses, sender identity from transport evidence, a shared MCP tool surface, and an A2A-shaped message model.

## Layout

- `docs/design.md`: the design document (components, registry, message schema, ingress contracts, injector per harness, trust model, tool surface, gating tests).
- `docs/decisions/`: architecture decision records.
- `docs/setup.md`: install on macOS or Linux.
- `docs/why.md`, `docs/examples.md`, `docs/how-it-works.md`, `docs/faq.md`: why it is useful, worked examples, a plain-English explanation and common questions.
- `CONTRIBUTING.md` (and `docs/contributing.md`): build, test and send a change.
- Site: https://getcrosstalk.dev (the same `docs/` pages, built by GitHub Pages).
- `plugins/crosstalk/`: the Claude Code and Codex plugin and its skill.

## License

MIT. See `LICENSE`.
