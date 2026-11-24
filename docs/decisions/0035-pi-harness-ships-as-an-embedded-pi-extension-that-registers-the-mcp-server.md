# ADR 0035 — Pi harness ships as an embedded Pi extension that registers the MCP server

- **Status:** Provisional (every ADR in this repo is provisional by operator ruling, 2026-11-04)
- **Date:** 2026-11-24
- **Deciders:** operator + Claude (Opus 5.5), Codex critique
- **Context source:** issue #275; [change plan](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/plans/pi-harness.md) sections 2 and 3; measurements on the installed Pi 1.6.0 (plan section 2, "Measured"); ADR 0006, ADR 0008, ADR 0013, ADR 0016, ADR 0031

## Context

Pi (the `@earendil-works/pi-coding-agent` npm package, 1.6.0 installed) has built-in MCP (`pi mcp add`, `~/.pi/agent/mcp.json`), but its MCP client sends no session id on `tools/call` (only `_meta.progressToken`) and has no inbound channel into a running session. Pi's extension API has both: an extension runs in the Pi process, can read `ctx.sessionManager.getSessionId()`, and can put a message into the session with `pi.sendMessage`. So part of the Pi harness must be a Pi extension (TypeScript, loaded by Pi through jiti with no build step), next to the Go package behind `harness.Harness` (ADR 0008). Issue #275 asks how that extension is shipped, versioned and checked.

An extension can also add the MCP server itself with `pi.registerMcpServer(name, config)`. Measured on 1.6.0: a server registered at extension load is spawned as a child of the Pi process when the session starts and is restarted on every session switch, the same as an `mcp.json` server. An `mcp.json` entry with the same name takes precedence over the registration, and `pi mcp` shell commands do not see registered servers.

## Decision

1. Pi 1.x is the supported line (1.6.0 tested); `TestedMajors = {1}`, an untested minor warns, another major is refused, the model of `codex/version.go`.
2. The Pi side is one file, `<agent dir>/extensions/crosstalk/index.ts`, where the agent dir is `$PI_CODING_AGENT_DIR` or `~/.pi/agent`. Its source lives in the repo at `internal/harness/pi/ext/index.ts`, is embedded in the binary with `go:embed`, and is written by `crosstalk install pi` with two values filled in: the installed binary's absolute path and the CrossTalk version. It is kept small: it registers the server, connects to the shim, forwards deliveries and session-name changes, and holds no CrossTalk logic (rendering, policy and state stay in Go).
3. The extension registers the CrossTalk MCP server itself (`pi.registerMcpServer("crosstalk", {command: <abs>/crosstalk, args: ["mcp"], env?, exposure: "direct"})`). `crosstalk install pi` writes no `mcp.json` entry. `CROSSTALK_NO_AUTO_REFRESH=1` goes in that `env` when install is given `--no-auto-refresh`.
4. Versioning is lockstep with the binary. The extension's version is the binary's version; it sends that version and a protocol number in its hello (ADR 0036). A protocol mismatch is refused with a reason; a version mismatch with the same protocol works and K14 warns. The upgrade follow-up (ADR 0031) rewrites the file through `PluginUpdater`, so the first shim on a new version refreshes it, carrying over the file's no-auto-refresh setting and using a follow-up marker scoped to the agent dir; running Pi sessions load the new file at their next `/reload` or start.
5. Uninstall deletes only CrossTalk's own files (`index.ts` with the marker line and the installed binary path, and its `.crosstalk-backup`), then the directory if it is empty; anything else is reported, not deleted. An `mcp.json` (global or project `.pi/mcp.json`) entry named `crosstalk` stops the install with `pi mcp remove crosstalk` named, because it would take precedence over the registration.
6. Checks: K2 (one CrossTalk server: the extension and no `crosstalk` entry in either `mcp.json`), K5 (the extension's binary path is the installed binary and its version is the binary's), K6 (a live handshake with that command), and a new warn-only K14 (Pi's version is tested, and `builtin:mcp` is not disabled in Pi's `settings.json`, without which the server never starts).

## Rejected alternatives

- **`pi mcp add` entry plus an extension.** Measured to work (`pi mcp add` is idempotent, keeps other keys, takes `--env`; `pi mcp list --json` reports state). Two artifacts that must both be present: an entry without the extension gives unbound sessions, and the extension without the entry gives no shim. The operator chose one file (plan section 10, Q2).
- **A Pi package from npm or git (`pi install git:github.com/GoodOlClint/CrossTalk@<tag>`).** Needs a network fetch and a release ref, and the package version can drift from the binary. Embedding makes the binary the single source.
- **No extension: identity from the parent pid only.** Gives no session id (Pi writes no live session registry) and no inbound channel.
- **Pi's RPC mode (`pi --mode rpc`).** A separate headless launch; the operator's TUI session is not reachable through it.

## Consequences

- `crosstalk install` writes TypeScript that Pi executes with the user's full permissions; the file is generated from embedded bytes, never fetched.
- A Pi session started with `--no-extensions`, `--no-mcp` or `-builtin:mcp` has no CrossTalk; K14 names the settings case.
- `pi mcp list` does not show CrossTalk; setup docs say to use `/mcp` inside a session or `crosstalk install --check`.
- ADR 0008 Consequences gain the Pi core edits (plan slice 1).

## Trial record

- Council: none sat; one-shot Codex critique recorded in the plan's "Review disposition" section.
- Rests on: ADR 0006, ADR 0008, ADR 0016, ADR 0031.
- Commits: none yet (plan stage).
- Pinning test: planned in the plan's slices 2 and 6.
