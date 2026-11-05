# crosstalk

Cross-harness agent messaging: the shim (`crosstalk mcp`, an MCP server the harness starts) is, per agent session it serves, an MCP server to that session's model, an A2A server and client to other shims, a Claude-compatible unix-socket ingress, and the injector into its own harness (Claude Code, Codex). Agents message each other by name; messages land inside the running session through the harness's native inbound channel, never by screen scraping.

## Architecture

- Go, single static binary (`crosstalk`). CrossTalk spawns and wraps nothing: `crosstalk mcp` is the shim, registered once per harness by `crosstalk install` at the installed binary's absolute path (Claude `claude mcp add --scope user crosstalk -- <abs>/crosstalk mcp`; Codex `[mcp_servers.crosstalk]` `command = "<abs>/crosstalk"`, `args = ["mcp"]`, `default_tools_approval_mode = "approve"`, which stops Codex from prompting on every CrossTalk tool call). ADR 0002, ADR 0006 (supersedes ADR 0005). Each harness is one package behind `harness.Harness`, compiled into the one binary (ADR 0008): shim, mcpserver and a2a import no harness package. Codex delivery as toolOutput: ADR 0007 (Codex-authored, branch codex/toolout). Native replies are bounced back to their own session, never routed: ADR 0009. Plugins and the skill: ADR 0013, tree under `plugins/crosstalk/`. No message limit, a nudge instead: ADR 0014. Signed macOS release binary built in CI on a version tag: ADR 0015, `.github/workflows/release.yml`, `scripts/release-build.sh`. `crosstalk install`, `uninstall` and `reinstall` (0.1.3, ADR 0016, `cmd/crosstalk/install.go` plus each harness's `install.go` behind the optional `harness.Installer`): copy the binary to `~/.local/bin`, install the plugin (skill only, pinned to a release tag), register the server at the absolute path, port Claude rules to the `mcp__crosstalk__` prefix and add the allow rule, optionally edit PATH (macOS `~/.zprofile`/`~/.zshenv`, Linux `~/.profile`/`~/.bashrc`), then run checks K1-K7 (`crosstalk install --check`); `CROSSTALK_MARKETPLACE` overrides the marketplace source for an rc rehearsal. Linux amd64 and arm64 release assets are built outside the signing path (0.1.3, ADR 0017, `docs/plans/release-0.1.3.md`).
- Identity per request, never per process: Claude from the parent pid (`~/.claude/sessions/<ppid>.json`), Codex from `params._meta.threadId` on each `tools/call`; neither → `unbound`. One process may serve several Codex threads (pooling); one agent record per (harness, session_ref).
- Model-facing: MCP over stdio, seven tools (`list_agents`, `send_message`, `reply`, `ask`, `receive_message`, `claim`, `release`).
- Peer-facing: A2A over loopback HTTP plus a `/tmp/cc-socks`-compatible unix socket speaking Claude Code's two-line peer protocol.
- Harness adapters: Claude (session socket + `~/.claude/sessions` registry), Codex (managed app-server daemon over websocket-on-unix: `turn/start.toolOutput`, `thread/inject_items`; [ADR 0007](docs/decisions/0007-codex-toolout-delivery.md)).
- State: per-agent append-only JSONL journal under `~/.local/state/crosstalk/journal/` (dedupe, dispatch claims, reply routing, claims), replayed when the agent registers.
- Discovery: every Claude session and loaded Codex thread from the harness registries, merged with `~/.local/state/crosstalk/agents/<agent_id>.json` descriptors; no daemon, no herdr dependency. ADR 0003.
- Trust: sender identity from transport (peer pid, bearer credential), never from message text; kinds `inform`, `ask` and `command` in v1 (`kind` is a routing hint, not a trust boundary), `control` deferred. ADR 0004.

## Messaging limits

There is no limit on how many messages a session sends, and nothing bounds spend (ADR 0014). Each send starts a fresh one-element hop chain; `hopCheck` on the receiver still guards an A2A client that supplies its own chain. Each agent counts its sends per peer and, at 30, 60, 120, 240 and so on, adds a nudge to the tool result asking the model to check whether the exchange is progressing (it refuses nothing); the count is in memory and resets on restart. It misses fan-out (one message to many peers, each below the threshold), restarts (the count resets), failed sends (not counted), `crosstalk send` and any client that goes around the shim, native bounces, and Claude's own SendMessage. Relay is not closed either: the notice does not stop a peer asking a session to message a third one, only the skill does (design §7). Design §4.

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

v1 implemented on main, with the agent plugins and the skill (ADR 0013; `docs/setup.md`); a first live clean-account run was done on 2026-11-04 with gaps (ADR 0013 "Not yet run live"; `docs/plans/agent-plugins-clean-run.md`). Wave 2 adds the per-peer nudge, `CROSSTALK_CODEX_ENDPOINTS`, the notice on pulled messages and `crosstalk send`. `docs/design.md` is still a draft under operator review, and its §11 tests gate "done" for v1 (test 17, the SDK conformance gate, is open). The release workflow has never run: its first real run is unproven until a `v*` tag is pushed with the signing secrets set (ADR 0015). CI (`.github/workflows/ci.yml`: build, `go test -race`, vet and a gofmt check on Linux and macOS) runs on push to main and on pull requests.

The repo is public (the operator flipped it on 2026-11-04) at https://github.com/GoodOlClint/CrossTalk; a stranger installs with no credentials (`docs/setup.md`). Commit author email stays as is; no history rewrite.

## Build / run / test

- Build: `go build ./...`
- Test: `go test ./...`
