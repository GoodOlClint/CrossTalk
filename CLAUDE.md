# crosstalk

Cross-harness agent messaging: one process per agent session (the shim) that is an MCP server to its own model, an A2A server and client to other shims, a Claude-compatible unix-socket ingress, and the injector into its own harness (Claude Code, Codex). Agents message each other by name; messages land inside the running session through the harness's native inbound channel, never by screen scraping.

## Architecture

- Go, single static binary (`crosstalk`), one shim process per session started by `crosstalk run -- <harness cmd>`; the harness's MCP entry is `crosstalk mcp`, a stateless stdio forwarder to that shim. ADR 0002.
- Model-facing: MCP over stdio (`list_agents`, `send_message`, `reply`, `ask`, `receive_message`).
- Peer-facing: A2A over loopback HTTP plus a `/tmp/cc-socks`-compatible unix socket speaking Claude Code's two-line peer protocol.
- Harness adapters: Claude (session socket + `~/.claude/sessions` registry), Codex (managed app-server daemon over websocket-on-unix: `thread/turns/list`, `turn/steer`, `turn/start`, `codex queue`).
- State: per-agent append-only JSONL journal under `~/.local/state/crosstalk/journal/` (dedupe, dispatch claims, reply routing), replayed on start.
- Discovery: `~/.local/state/crosstalk/agents/<name>.json` descriptors enriched from each harness's live registry; no daemon, no herdr dependency. ADR 0003.
- Trust: sender identity from transport (peer pid, bearer credential), never from message text; kinds `inform`, `ask` and `command` in v1 (`kind` is a routing hint, not a trust boundary), `control` deferred. ADR 0004.

## Canonical pipelines

- All inbound messages, from either ingress, flow through one receiver chain: authenticate → dedupe → hop-chain → policy → inject. Do not inject from an ingress handler directly.
- All harness-specific behaviour lives in the adapter for that harness (`register`, `inject`, `status`). No harness branching elsewhere.

## Public surface

- The MCP tool surface in `docs/design.md` §8.
- The descriptor schema in `docs/design.md` §3.
- The message schema in `docs/design.md` §4.

## ADRs

Read `docs/decisions/` before architectural changes. `docs/design.md` is the design document; its §11 tests gate "done" for v1.

## Status

Design phase. No implementation code until the design doc is approved by the operator (greenfield gate).

## Build / run / test

- Build: `go build ./...`
- Test: `go test ./...`
