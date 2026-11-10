# crosstalk

Cross-harness agent messaging: the shim (`crosstalk mcp`, an MCP server the harness starts) is, per agent session it serves, an MCP server to that session's model, an A2A server and client to other shims, a Claude-compatible unix-socket ingress, and the injector into its own harness (Claude Code, Codex). Agents message each other by name; messages land inside the running session through the harness's native inbound channel, never by screen scraping.

## Architecture

- Go, single static binary (`crosstalk`). CrossTalk spawns and wraps nothing: `crosstalk mcp` is the shim, registered once per harness by `crosstalk install` at the installed binary's absolute path (Claude `claude mcp add --scope user crosstalk -- <abs>/crosstalk mcp`; Codex `[mcp_servers.crosstalk]` `command = "<abs>/crosstalk"`, `args = ["mcp"]`, `default_tools_approval_mode = "approve"`, which stops Codex from prompting on every CrossTalk tool call). ADR 0002, ADR 0006 (supersedes ADR 0005). Each harness is one package behind `harness.Harness`, compiled into the one binary (ADR 0008): shim, mcpserver and a2a import no harness package. Codex delivery as toolOutput: ADR 0007 (Codex-authored, branch codex/toolout). Native replies are bounced back to their own session, never routed: ADR 0009. Plugins and the skill: ADR 0013, tree under `plugins/crosstalk/`. No message limit, a nudge instead: ADR 0014. Signed macOS release binary built in CI on a version tag: ADR 0015, `.github/workflows/release.yml`, `scripts/release-build.sh`. `crosstalk install`, `uninstall` and `reinstall` (0.1.3, ADR 0016, `cmd/crosstalk/install.go` plus each harness's `install.go` behind the optional `harness.Installer`): copy the binary to `~/.local/bin` (a linked Homebrew binary is used in place, ADR 0018), install the plugin (skill only, pinned to a release tag), register the server at the absolute path, port Claude rules to the `mcp__crosstalk__` prefix and add the allow rule, optionally edit PATH (macOS `~/.zprofile`/`~/.zshenv`, Linux `~/.profile`/`~/.bashrc`), then run checks K1-K9 (`crosstalk install --check`); `CROSSTALK_MARKETPLACE` overrides the marketplace source for an rc rehearsal. Linux amd64 and arm64 release assets are built outside the signing path (0.1.3, ADR 0017, `docs/plans/release-0.1.3.md`). Shipped in 0.1.4 (`docs/plans/release-0.1.4.md`): a Homebrew formula in `GoodOlClint/homebrew-tap` from the release assets, with `crosstalk install` registering `<brew prefix>/bin/crosstalk` for a Homebrew binary (ADR 0018), and both marketplace refs moved to each final tag by a PR from `release.yml` that `land-pr` merges once required checks pass (ADR 0019); both jobs mint a GitHub App token from the `homebrew-tap` environment and ran live for v0.1.4 and v0.1.5. A docs site at `https://getcrosstalk.dev`, built from `docs/` by GitHub's Jekyll action (ADR 0020).
- Remote, planned for v0.2.0, not built (`docs/plans/release-0.2.0.md`): messaging between the same user's own machines over a LAN or tailnet. One opt-in gateway per host, `crosstalk remote serve`, started by the user or by a login unit the user turns on (`crosstalk remote enable`, `crosstalk install --remote`); never installed or started by default. TLS 1.3 pinned mTLS, two-sided pairing (`remote pair` / `remote join`), default port 7745, static IPs (ADR 0021). Addressing `name@host`, bare names local-only, host-scoped sender keys `remote:<fingerprint>/<agent_id>`, cross-host `inform`/`ask`/`reply` only, paired hosts' agents listed live in `list_agents` (ADR 0022). Amends ADRs 0001, 0003, 0004, 0006 and 0011 (paragraphs marked planned).
- Codex `list_agents` rows carry `backend` (`reachable`, `unreachable`, `unknown`), the hosting app-server's reachability to its OpenAI backend from `account/rateLimits/read`; a send to an `unreachable` one carries a non-refusing hint, and `crosstalk install --check` has the warn-only K9. ADR 0025.
- Role inboxes (ADR 0024, built except the fallback walk past an unreachable responder): a send to `inbox:<name>` reaches the newest member, the responder, and gives every other member a copy that opens no route; membership is in descriptors, ownership and grants in `inboxes/<name>.json`.
- Identity per request, never per process: Claude from the parent pid (`~/.claude/sessions/<ppid>.json`), Codex from `params._meta.threadId` on each `tools/call`; neither → `unbound`. One process may serve several Codex threads (pooling); one agent record per (harness, session_ref).
- Model-facing: MCP over stdio, tools `list_agents`, `send_message`, `reply`, `ask`, `receive_message`, `claim`, `release`, `rename` (a session sets its own name, pinned against harness renames and kept across restarts: ADR 0027), `attach`, `detach` and `grant` (role inboxes: ADR 0024).
- Peer-facing: A2A over loopback HTTP plus a `/tmp/cc-socks`-compatible unix socket speaking Claude Code's two-line peer protocol.
- Harness adapters: Claude (session socket + `~/.claude/sessions` registry), Codex (managed app-server daemon over websocket-on-unix: `turn/start.toolOutput`, `thread/inject_items`; [ADR 0007](docs/decisions/0007-codex-toolout-delivery.md)).
- State: per-agent append-only JSONL journal under `~/.local/state/crosstalk/journal/` (dedupe, dispatch claims, reply routing, claims), replayed when the agent registers. Informs injected into a session before it binds are kept in a signed pre-bind store under `~/.local/state/crosstalk/prebind/` and replayed into that journal at bind, so `reply` finds them (ADR 0026, `internal/prebind`).
- Discovery: every Claude session and loaded Codex thread from the harness registries, merged with `~/.local/state/crosstalk/agents/<agent_id>.json` descriptors; no daemon, no herdr dependency. ADR 0003. Planned for v0.2.0: paired hosts' agents fetched live from their gateways, never written to the descriptor directory (ADR 0022).
- Trust: sender identity from transport (peer pid, bearer credential), never from message text; kinds `inform`, `ask` and `command` in v1 (`kind` is a routing hint, not a trust boundary), `control` deferred. ADR 0004. Planned for v0.2.0: a remote sender's principal is the pinned host fingerprint, and it may not `command`, `claim` or `release` (ADR 0022).

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

v1 implemented on main, with the agent plugins and the skill (ADR 0013; `docs/setup.md`); a first live clean-account run was done on 2026-11-04 with gaps (ADR 0013 "Not yet run live"; `docs/plans/agent-plugins-clean-run.md`). Wave 2 adds the per-peer nudge, `CROSSTALK_CODEX_ENDPOINTS`, the notice on pulled messages and `crosstalk send`. `docs/design.md` is still a draft under operator review, and its §11 tests gate "done" for v1 (test 17, the SDK conformance gate, is open). The release workflow has published v0.1.2 through v0.1.5 signed and notarized (ADR 0015). CI (`.github/workflows/ci.yml`: build, `go test -race`, vet and a gofmt check on Linux and macOS) runs on push to main and on pull requests; a change that touches only `*.md`, `docs/**` or the two marketplace files runs only the fast docs, setup, manifest, marketplace and version tests (`release-build.sh ci-scope`), and the required check names are still reported.

The repo is public (the operator flipped it on 2026-11-04) at https://github.com/GoodOlClint/CrossTalk; a stranger installs with no credentials (`docs/setup.md`). Commit author email stays as is; no history rewrite.

Delivery (operator ruling, 2026-11-05): `main` is protected by a ruleset (required checks `test (ubuntu-latest)` and `test (macos-latest)`, a pull request for every change, no force-push or deletion; the operator can bypass). For 0.1.4 work, agents may push branches, open pull requests and merge them once CI is green without asking each time; release tags still need the operator's yes.

Standing work permission (operator, 2026-11-07): the coordinator chooses the next v0.2.0 work and launches workers without asking each time, to keep development moving; implementation may go to Codex workers as well as Claude workers. Release tags and anything outside the repo still need the operator.

## Agent sessions on this repo

Sessions working this repo (coordinator, workers, discussion sessions) message each other with CrossTalk (`list_agents`, `send_message`, `ask`, `reply`), not `hc send` (operator ruling, 2026-11-06): `hc send` types into the terminal as the operator and has cut into the operator's typing. `hc` stays for launching, waiting, reading screens and retiring terminals. A worker launch is checked with `list_agents`: the worker must show `crosstalk: true`, or the coordinator tells the operator before relying on it. A kickoff gives the coordinator's display name from the handoff file's CURRENT line as the `send_message` target.

Dogfood (operator ruling, 2026-11-06): every agent-to-agent exchange on this repo that CrossTalk can carry goes over CrossTalk, including Codex reviews and council sessions. A Codex review is an `ask` to the standing Codex reviewer session (`crosstalk-codex-review` in hc; brief `~/.claude/kickoffs/crosstalk-codex-reviewer-2026-11-06.md`) naming the worktree path and base ref; it answers with `reply`. A council runs its advisors as CrossTalk peer sessions, at least one of them Codex, rather than in-process subagents. The `codex-companion.mjs` runtime is the fallback only when the reviewer is unreachable or silent for 30 minutes, and the report says so. Each CrossTalk failure met this way (lost message, wrong name, no reply) is filed as an issue with the `dogfood` label, not worked around silently; a coordinator files these without asking (operator, 2026-11-07).

Public issues (operator ruling, 2026-11-07): the repo is public, so an issue, comment or PR body names no session or coordinator, terminal, agent_id, message-id, local username, home path, host name, IP or other project. Describe roles generically ("a Claude session", "the receiver") and paths as `~/...`; keep identifying detail in the handoff file or the ledger.

Pushes (operator, 2026-11-06): agent sessions push branches with the `push-verified` MCP tool (`push_verified`), not `git push`. Pass `repo_path` `/Users/goodolclint/Source/crosstalk` (the main checkout) even from a `wt` worktree: the tool refuses sibling worktree paths as outside its repo root, and the branch is shared through the one object store. It needs no permission prompt, so a worker does not stop on the `Bash(git push:*)` ask rule, and its commits land Verified as the GitHub App. It re-authors each commit and resets the local branch to the new OIDs, so report the sha after the push. `git push` is the fallback only for what the tool refuses (see its description), or a change under `.github/workflows/` if the App lacks the workflows permission; say so in the report.

## Build / run / test

- Build: `go build ./...`
- Test: `go test ./...`
