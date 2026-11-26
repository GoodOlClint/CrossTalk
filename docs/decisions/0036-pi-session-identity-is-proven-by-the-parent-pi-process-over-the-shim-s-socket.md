# ADR 0036 — Pi session identity is proven by the parent Pi process over the shim's socket

- **Status:** Provisional (every ADR in this repo is provisional by operator ruling, 2026-11-04)
- **Date:** 2026-11-24
- **Deciders:** operator + Claude (Opus 5.5), Codex critique
- **Context source:** issue #275 ("how the shim ties an MCP request to the Pi session that made it"); [change plan](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/plans/pi-harness.md) section 2 "Measured" and D2 to D4; ADR 0003, ADR 0004, ADR 0006, ADR 0035; AGENTS.md "Identity per request, never per process"

## Context

AGENTS.md says identity is per request, never per process: Claude from the parent pid's session file, Codex and OpenCode from `_meta` on each `tools/call`. Pi sends no session id on `tools/call`. Measured on Pi 1.6.0 (macOS, scratch profile, no model):

- The MCP server's parent pid is the Pi process, the same pid as the extension's `process.pid`.
- Pi starts its MCP servers on `session_start` and stops them on `session_shutdown`; `/new`, `/resume`, `/fork`, `/reload` and quit each shut the old session's servers down (stdin closed, then SIGTERM) before the next session starts and spawns new ones. So one `crosstalk mcp` process serves exactly one Pi session for its whole life.
- The extension's `session_start` runs before the MCP server is spawned; the extension can connect to a unix socket the server opens, retrying until it exists (connected on the second attempt, 200 ms apart).
- On macOS the server reads the connecting peer's pid with `LOCAL_PEERPID`; the pid was the Pi process, equal to the server's own parent pid. Linux has `SO_PEERCRED` (`internal/peercred`).

## Decision

1. The Pi harness package implements `harness.ClaimSource`. Its `ServeClaims` runs only when the shim's parent looks like Pi (`PI_CODING_AGENT=true` in the shim's environment, which Pi sets and its MCP children inherit); otherwise it returns at once. It listens on `~/.local/state/crosstalk/sock/pi/<parent pid>.sock` (a 0700 directory, the K12 limit applies), replacing a stale file.
2. A connection is accepted only when the peer pid read from the socket (`internal/peercred`) equals the shim's parent pid and that parent is a Pi host (its command name is `pi`, the `process.title` Pi sets); any other peer is closed without a read. The extension instance generates a random token at load and gives it to this server registration only (`CROSSTALK_PI_TOKEN` in the `registerMcpServer` env); the hello must carry it, which binds the connection to the shim that registration started. The hello line also carries the protocol number, extension version, Pi version, session id (Pi's own id rule, not UUID only), session file path, working directory and session name. The session ref is `<session id>@<8 hex of sha256(session file path)>` (the bare id with no file), because Pi scopes an explicit `--session-id` to a directory while CrossTalk keys on (harness, session ref). The extension's `session_start` handler returns at once and connects in the background, because Pi awaits handlers in turn and starts MCP connections in its own handler. The shim registers that session through the `SessionRegistrar` (as Claude's `ServeClaims` does), so a Pi session is listed before it calls a tool.
3. The session id comes from the hello, and the hello is trusted because the transport proved the sender is the Pi process that spawned this shim. Message text is never an identity source (ADR 0004).
4. `Identify` runs on every call: it claims the call when `req.ParentPID` is the proven parent and a hello is held, and returns that session; otherwise `ok=false` (the call is unbound). The identity is therefore per process, and that is exact for Pi because the shim's life is one session. AGENTS.md's rule gains this exception in plan slice 1.
5. The connection stays open: it is the delivery channel (ADR 0037). The shim acks a good hello with `{"hello":"ok"}`. A refused hello (bad token, protocol mismatch, a different session id, a second claimant refused with `ErrAgentHeld`) gets an `{"error":"<reason>"}` line naming the reason, and the shim closes that connection. When the connection drops, the extension reconnects with backoff (200 ms growing to 2 s, reset only on the ack) while its session lives and sends the hello again; a refusal's reason is shown once in the footer and the extension keeps retrying. A hello naming a different session id than the one registered on this shim, on the same connection or a new one, is refused and logged, answered with `{"error"}` and its connection closed, since the measured lifecycle never produces it. After the hello the extension sends `{"status":"busy"|"idle"}` lines, which update the status file (point 6), and `{"name"}` lines (point 6).
6. No `Supersession` (the shim ends with the session). Every shim's `Discover` lists the live Pi sessions from the registry directory, with reachability and busy or idle status from a 0600 status file the owning shim keeps beside its socket, because a descriptor-only row reads as unreachable to other sessions and to paired hosts. And `Register` refuses a native-name send to a Pi session that has no CrossTalk (none are discoverable). A session name change (`session_info_changed`) is sent as a name line; a name the session set with `rename` stays pinned (ADR 0027).

## Rejected alternatives

- **The extension listens and the shim connects (the issue's first idea).** Measured to work, but Node has no peer-credential call, so the extension could not tell its own shim from any other same-user process writing text into the session. With the shim listening, the shim proves the peer with the kernel's peer pid.
- **A session file keyed by pid written by the extension, read by the shim (Claude's pattern).** Works for identity but needs a second channel for delivery and a file to clean up; the socket gives both.
- **The session id in the MCP server's `env`, set by `pi.registerMcpServer` in `session_start`.** Environment is not transport evidence, and it would need the server registered after the session starts.
- **Asking the model for its session id.** Any prompt-injected model could claim another session.

## Consequences

- Registration gives the local-teammate notice class, so the host check and the token are what keep an arbitrary launcher out of it; a same-user process can still fake both (a process titled `pi`, an env it chooses). A same-user process can also create a socket at the path before the shim does and feed the extension text. Both are inside ADR 0004's same-user model (any same-user process can already type into the TUI or edit the session file). The 0700 directory excludes other users.
- Two Pi processes that open the same session file make two shims claim one `(pi, session id)`; measured (spike 24 M4): Pi neither locks nor refuses, so both run, and the registry's live-holder rule keeps the first and refuses the later claimant (`ErrAgentHeld`); no takeover.
- `crosstalk mcp` started by hand inside a Pi `bash` tool call has the bash process as its parent, so it is never proven and stays unbound.

## Trial record

- Council: none sat; one-shot Codex critique recorded in the plan's "Review disposition" section.
- Rests on: ADR 0003, ADR 0004, ADR 0006, ADR 0035.
- Commits: none yet (plan stage).
- Pinning test: planned in the plan's slice 3 (peer pid other than the parent closed without a read; hello for a second session id refused; `Identify` ok only for the proven parent).
