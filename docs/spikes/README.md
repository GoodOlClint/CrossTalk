# Spikes

Pre-build experiments that back every "verified" claim in `docs/design.md`. Each spike gets one file `2026-11-03-<slug>.md` with the procedure, harness versions, outcome and captured frames (tokens redacted).

Status of captured frames: to be committed before `go mod init`. Until then the entries below are placeholders; outcomes are as recorded in the 2026-11-03 design-session notes.

## Completed (2026-11-03)

| Spike | Description | Captured frames |
|---|---|---|
| 1 | Claude Code inbound: two-line auth/user protocol on `/tmp/cc-socks/<pid>.sock` delivers a peer message mid-turn between tool calls (claude 2.1.317). | to be committed before `go mod init` |
| 2 | Codex inbound: `initialize` → `thread/turns/list` → `turn/steer` on a TUI-attached thread renders mid-turn (codex-cli 0.166.0). | to be committed before `go mod init` |
| 3 | Codex next-turn delivery: `codex queue --remote <addr> --thread <id> --message <text>` against a non-default app-server is delivered and answered. | to be committed before `go mod init` |
| 4 | Codex managed daemon: a plain `codex` pane appears in daemon `thread/loaded/list`; `codex queue --thread <id>` without `--remote` is delivered at the next turn boundary. | to be committed before `go mod init` |
| Round trip | Cross-harness round trip with native primitives only: Claude → `codex queue` → Codex writes the envelope to a Claude session socket → arrives as a `<cross-session-message>`. | to be committed before `go mod init` |

## Pending (precede the build steps named)

| Spike | Description | Precedes |
|---|---|---|
| 5 | macOS peer pid: read `LOCAL_PEERPID` (`SOL_LOCAL`) from Go via `golang.org/x/sys/unix` on an accepted uds connection and map it to a Claude session pid and to another shim's pid. | uds ingress |
| 6 | Idle Claude wake: inject into a Claude session with no active turn and record whether a turn starts and the model sees the envelope. | Claude adapter idle path (test 19) |
| 7 | Claude-visible record (R2): publish a `~/.claude/sessions/<pid>.json` + key pair for a shim and record what Claude does with it. | §3 decision, uds ingress (test 11) |
| 8 | Codex thread binding under `crosstalk run`: map a wrapper-launched Codex child to its thread id. | Codex adapter |
