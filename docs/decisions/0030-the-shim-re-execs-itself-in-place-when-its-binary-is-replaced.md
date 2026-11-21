# ADR 0030 — The shim re-execs itself in place when its binary is replaced

- **Status:** Proposed
- **Date:** 2026-11-21
- **Deciders:** operator (interview 2026-11-21, Q1 and Q2) + Claude (Opus 5.5)
- **Context source:** `docs/plans/upgrade-refresh.md`; #133 and its PR #200 (K11); spike `docs/spikes/20-upgrade-refresh.md`

## Context

A harness starts `crosstalk mcp` once and keeps the process for the life of the session (Claude) or the thread (Codex). An upgrade replaces the file on disk, and every running shim keeps the old code. #133 found about two dozen old shims on one host after an upgrade. K11 (PR #200, `cmd/crosstalk/install_shims.go`) lists them by parent and prints the restart for each, but it signals no process. The restart it names for the managed Codex daemon, `codex app-server daemon restart`, interrupts running Codex turns. `config/mcpServer/reload` does not respawn a running shim (`docs/plans/stale-shims-133.md`).

Measured on 2026-11-21 with a scratch stdio MCP probe (spike 20), Claude Code 2.1.330 and codex-cli 0.171.0:

- Re-exec in place (`execve` of the same pid, stdin and stdout kept, the initialized state carried across): Claude kept calling the server, fetched `tools/list` again on `notifications/tools/list_changed` and offered the new tool to the model. Codex kept calling the server and got the new code, but it did not fetch `tools/list` again; a new thread started a fresh server with the full list.
- Exit: Claude started the server again on the next call (new pid, new `initialize`) but did not fetch the tool list again. Codex answered every later call on that thread with `Transport closed`, and the thread had no CrossTalk until a new thread started.

The Go SDK can restore a session without a new `initialize`: `Server.Connect` takes `ServerSessionOptions.State`, and a request is accepted once `State.InitializeParams` is set (go-sdk v1.8.0 `mcp/server.go:1418-1432`, `1942-1991`). Its `StdioTransport` reads through a `json.Decoder` (`mcp/transport.go:524`), which reads ahead; bytes read ahead are lost on `execve`. `IOTransport` takes any reader (`mcp/transport.go:160`).

Identity is per request (ADR 0006): the Claude parent pid does not change on exec, and Codex sends `_meta.threadId` on each call. Journals, claims, pins and pre-bind entries are on disk.

## Decision

- `crosstalk mcp` re-execs itself in place when the file at its own launch path (`os.Args[0]`, the absolute path install registered) is no longer the file it runs. The file is identified by device and inode, as K11 does (`procFile`, `idOf`), and is checked on a 2 s tick (the Claude claim poll's period, ADR 0006).
- It re-execs only when idle: no tool call in flight, no delivery or injection in progress. A busy shim checks again on the next tick.
- Before `execve`, it runs the new file's `version` with a short timeout. A failure, or a version string that does not parse, means no exec: the shim keeps running the old code and logs the reason once per inode.
- The MCP session state (`InitializeParams`, log level) goes to the new process in an inherited, unlinked file named by an environment variable; the new process connects with `ServerSessionOptions.State` and sends `notifications/tools/list_changed`.
- Stdin is read through `IOTransport` with a reader that never reads past the end of the current line, so no request is lost on exec.
- Every other resource (A2A listener, unix socket, journal locks, Codex app-server connections) closes on exec through close-on-exec and is opened again by the new process as at start; the new process re-registers its sessions through the existing eager-registration and per-request paths.
- On by default (operator Q1). `CROSSTALK_NO_AUTO_REFRESH=1` in the server's environment turns it off; `crosstalk install --no-auto-refresh` writes that variable into both registrations. K11 keeps listing a shim that has not refreshed.
- CrossTalk never restarts the managed Codex daemon or any other process (operator Q2). K11 still names the daemon restart as advice, now only for shims that cannot refresh (refresh turned off, or an old shim from before this change).

## Rejected alternatives

- **Exit and let the harness start the shim again.** Codex does not: the thread has no CrossTalk until a new thread starts (spike 20). Claude does, but keeps the old tool list.
- **`codex app-server daemon restart` from install.** Interrupts running Codex turns; the operator ruled never (Q2).
- **`config/mcpServer/reload` from install.** Measured in #133: it does not respawn a running shim.
- **Opt-in.** The operator chose on by default (Q1); opt-in leaves the #133 state on every host that does not know about the flag.
- **Hand the read-ahead buffer to the new process instead of an exact line reader.** It works but adds a second channel to the exec; the line reader costs one small read loop.

## Consequences

- A Claude session gets the new code and the new tools within a few seconds of an upgrade. A Codex thread gets the new code; tools added by the new version appear only in threads started after the upgrade. K11 says so for Codex parents.
- During the exec gap (milliseconds to a second) the shim's A2A port and unix socket are closed. A peer send in the gap gets `unreachable` or a retry; dedupe in the journal makes a retry safe. The A2A port may change; the descriptor is rewritten as at start.
- A new binary that passes `version` but fails later in start leaves that session without CrossTalk until it restarts. The `version` check narrows this; it does not close it.
- The per-peer nudge count (ADR 0014) resets on refresh, as on a restart.
- The `cmd/crosstalk` MCP start moves from `srv.Run(ctx, &mcp.StdioTransport{})` (`cmd/crosstalk/main.go:132`) to `Connect` over `IOTransport`.
- A downgrade (the file replaced by an older version) refreshes the same way: the shim runs what is installed.
