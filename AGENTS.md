# crosstalk

Cross-harness agent messaging: the shim (`crosstalk mcp`, an MCP server the harness starts) is, per agent session it serves, an MCP server to that session's model, an A2A server and client to other shims, a Claude-compatible unix-socket ingress, and the injector into its own harness (Claude Code, Codex). Agents message each other by name; messages land inside the running session through the harness's native inbound channel, never by screen scraping.

## Architecture

- Go, single static binary (`crosstalk`). CrossTalk spawns and wraps nothing: `crosstalk mcp` is the shim, installed once per harness (`claude mcp add --scope user crosstalk -- crosstalk mcp`; Codex `[mcp_servers.crosstalk]` `command = "crosstalk"`, `args = ["mcp"]`, `default_tools_approval_mode = "approve"`, which stops Codex from prompting on every CrossTalk tool call). ADR 0002, ADR 0006 (supersedes ADR 0005). Each harness is one package behind `harness.Harness`, compiled into the one binary (ADR 0008): shim, mcpserver and a2a import no harness package. Codex delivery as toolOutput: ADR 0007 (Codex-authored, branch codex/toolout). Native replies are bounced back to their own session, never routed: ADR 0009.
- Identity per request, never per process: Claude from the parent pid (`~/.claude/sessions/<ppid>.json`), Codex from `params._meta.threadId` on each `tools/call`; neither → `unbound`. One process may serve several Codex threads (pooling); one agent record per (harness, session_ref).
- Model-facing: MCP over stdio, seven tools (`list_agents`, `send_message`, `reply`, `ask`, `receive_message`, `claim`, `release`).
- Peer-facing: A2A over loopback HTTP plus a `/tmp/cc-socks`-compatible unix socket speaking Claude Code's two-line peer protocol.
- Harness adapters: Claude (session socket + `~/.claude/sessions` registry), Codex (managed app-server daemon over websocket-on-unix: `turn/start.toolOutput`, `thread/inject_items`; [ADR 0007](docs/decisions/0007-codex-toolout-delivery.md)).
- State: per-agent append-only JSONL journal under `~/.local/state/crosstalk/journal/` (dedupe, dispatch claims, reply routing, claims), replayed when the agent registers.
- Discovery: every Claude session and loaded Codex thread from the harness registries, merged with `~/.local/state/crosstalk/agents/<agent_id>.json` descriptors; no daemon, no herdr dependency. ADR 0003.
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

Every ADR in this repo is provisional (operator ruling, 2026-11-04). An ADR may be edited in place, with no supersession step and no operator hard stop, when the design or code moves; keep it matching what the code does.

## Status

v1 implemented on main. `docs/design.md` is still a draft under operator review, and its §11 tests gate "done" for v1 (test 17, the SDK conformance gate, is open).

The repo is private. It goes public only after CI is in place (operator ruling, 2026-11-04), and after username paths and the stale README are fixed at HEAD. The operator flips the visibility. Commit author email stays as is; no history rewrite.

## Build / run / test

- Build: `go build ./...`
- Test: `go test ./...`
