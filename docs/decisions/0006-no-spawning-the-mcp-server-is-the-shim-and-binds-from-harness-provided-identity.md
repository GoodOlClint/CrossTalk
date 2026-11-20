# ADR 0006 — No spawning: the MCP server is the shim and binds from harness-provided identity

- **Status:** Provisional
- **Date:** 2026-11-03
- **Deciders:** operator + Claude session + Codex session
- **Context source:** operator redirect 2026-11-03 ("build this without needing to spawn processes on our own; communicate with any Claude or Codex session running"); Codex source read of rust-v0.166.0 `codex-rs/core/src/mcp_tool_call.rs`; live process inspection on this machine

## Context

ADR 0005 made `crosstalk run` the process owner because an MCP server process could not tell which session it served. That forced every session to be launched through CrossTalk. Two facts remove the need. Claude Code starts MCP servers as children of the session process, so the server's parent pid names the session and `~/.claude/sessions/<ppid>.json` carries its name, socket and status. Codex attaches `params._meta.threadId` and `sessionId` to every MCP `tools/call` it issues (`with_mcp_tool_call_ids_meta`), and the app-server's own `mcpServer/tool/call` sets `_meta.threadId` too. Codex 0.166 can also pool one stdio MCP connection across threads when the 2026-07-28 protocol is negotiated, so a process is not reliably one-per-thread.

Inbound delivery already needs nothing installed on the receiver: Claude's per-session socket and Codex's app-server reaches any running session (then `turn/steer` and `codex queue`; now `turn/start` `toolOutput` and `thread/inject_items`, ADR 0007).

## Decision

At runtime CrossTalk spawns nothing and wraps nothing (`crosstalk install` runs the harness CLIs to register the server, ADR 0016; that is setup, not messaging). `crosstalk mcp` is the shim, installed once per harness as an ordinary MCP server entry. It serves whatever sessions the harness connects to it and binds identity per request: Claude from the parent pid; Codex from `_meta.threadId` on each `tools/call`. A request with neither is `unbound` and is served `list_agents` only. One process may hold several agent records (Codex pooling); nothing is bound permanently to the first caller.

Every running session is listed by `list_agents` whether or not it has CrossTalk loaded, from `~/.claude/sessions` and `thread/loaded/list` on known app-servers, with `reachable` and `crosstalk` flags. A session without CrossTalk can receive but cannot send, has no stable name beyond its harness name, and has no A2A card.

Role and owner are set at runtime by `claim` and `release` between registered agents, not by a launcher. `crosstalk run` is removed from v1.

Eager Claude registration (operator ruling, 2026-11-20; #145): a Claude session registers when its shim starts, not on its first CrossTalk call. Before this the agent record was created lazily on the first bound `tools/call`, so a session whose model had not yet called CrossTalk stayed a native row: `crosstalk: false`, asks and commands to it rejected `native_inform_only`, informs held in the pre-bind store (ADR 0026), no role-inbox membership. The shim runs from session start and its identity is its parent pid on every request, so the Claude harness implements `harness.ClaimSource`: `ServeClaims` reads its own parent's session file, `~/.claude/sessions/<ppid>.json`, every 2 s (`claude.DefaultPollInterval`) and calls `RegisterSession` whenever no live descriptor holds that session under its harness name: when the file first appears (it may not exist yet at shim start), when `/clear` or `/resume` names another session or brings back one that was retired, when the harness renames the session, and while it is registered under a fallback name because the harness name was held (for example by a superseded agent still draining), so the session takes its name once it is free without a call. A failed registration is retried on every poll and logged once per session, name and error. The claim-source loop in `cmd/crosstalk` starts it; `mcpserver` is unchanged, and a registration that races the first `tools/call` for the same session yields one agent record, listener, journal and lock. No hook is added; a `SessionStart` hook was considered and rejected because the shim is already running and needs no owner proof, so a hook would only nudge a process that can poll by itself, at the cost of install, uninstall and check steps. Not covered: a notice produced by an eager registration (a role inbox dropped while the session was down, a fallback name) is not shown to the model, as with the ADR 0028 claim path; the model sees the current state in `list_agents`. Accepted costs: every Claude session holds a descriptor, its sockets and an A2A port, including a short `claude -p` run. With duplicate installs, the first shim to start holds the agent, not the first caller; the second sees the first's descriptor and does not register, and its calls get `rejected/agent_held`. A session whose name is pinned by `rename` (ADR 0027) differs from its harness name, so its shim calls `RegisterSession` on every poll; that call reads the session file and moves nothing. Codex is unchanged: a daemon session's identity still arrives per call in `_meta.threadId`, and `--no-daemon` keeps the ADR 0028 hook.

Built in 0.2.0 ([ADR 0021](0021-remote-transport-and-enrollment-a-user-started-per-host-gateway-with-pinned-mtls-and-two-sided-pairing.md)): one opt-in process outside the shim is allowed, the remote gateway `crosstalk remote serve`, which the user starts in a terminal or through a login unit the user turns on (`crosstalk remote enable`, `crosstalk install --remote`, or a yes to install's question, default no). Nothing is installed or started by default. The shim still spawns nothing: it neither starts nor supervises the gateway, and it works without one (it then cannot receive from other hosts).

## Rejected alternatives

- **ADR 0005 wrapper (`crosstalk run`).** Superseded. It required launching every session through CrossTalk, which the operator rejected; its binding problem is solved by harness-provided identity.
- **Activity inference (the single active thread is the caller).** Rejected by both sessions: concurrent active threads and status races defeat it, and an app-server tool call need not come from an active turn.
- **Nonce handshake via `mcpServer/tool/call`.** Workable but unnecessary once `_meta.threadId` is present on ordinary calls; kept as a diagnostic (`crosstalk capture`), not as the binding.
- **Environment or cwd inheritance.** Dead: Codex MCP children carry only `CODEX_HOME` and `CODEX_CLI_PATH`; cwd is the configured or fallback value.

## Consequences

- Install registers one MCP server entry per harness, done by `crosstalk install` at the installed binary's absolute path ([ADR 0016](0016-crosstalk-install-registers-mcp-at-the-installed-path.md)); plain `claude` and plain `codex` work. An agent host such as Herdr needs no CrossTalk knowledge.
- The Codex `_meta` contract is release-source evidence, not a documented API, re-verified per Codex release. 0.166 is the tested minor (`TestedMinors` in `internal/harness/codex/version.go`): another 0.x minor warns and continues, and an untested major is refused. Spike 9 captured `_meta.threadId` and `sessionId` live on a model-issued call on 0.166.0 before the Codex adapter was written ([report](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/spikes/09-codex-binding/report.md)).
- A pooled Codex connection means per-request agent lookup and no process-level "my session" state; the journal, descriptor and name-stable socket are per agent, not per process.
- A session whose shim process has exited (MCP connection closed) is still reachable natively but its name-stable socket has no listener until the harness reconnects the MCP server; design.md §9 states this.
- Supersedes ADR 0005.

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (no spawning scoped to runtime; install via ADR 0016; Codex version check wording; spike 9 report linked). Status stays Provisional.
