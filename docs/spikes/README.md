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
| 5 | macOS peer pid: `LOCAL_PEERPID` (`SOL_LOCAL`) read from Go on an accepted uds connection returns the dialing process's pid, same process and another process (`internal/peercred`). | [05-peerpid.md](05-peerpid.md) |
| 9 | Harness-provided identity (ADR 0006): a Codex 0.166.0 `tools/call` carries `params._meta.threadId` (and `sessionId` on a model-issued call). | [09-codex-binding/report.md](09-codex-binding/report.md) |

## Pending (precede the build steps named)

| Spike | Description | Precedes |
|---|---|---|
| 6 | Idle Claude wake: inject into a Claude session with no active turn and record whether a turn starts and the model sees the envelope. | Claude adapter idle path (test 19) |
| 7 | Claude-visible record (R2): publish a `~/.claude/sessions/<pid>.json` + key pair for a shim and record what Claude does with it. | §3 decision, uds ingress (test 11) |

- [11 harness landscape](11-harness-landscape.md) — which other harnesses can take in-session delivery; recommended early set OpenCode, Gemini CLI (+Qwen Code), Pi; ACP is not an attach surface.
- [12 Antigravity CLI](12-antigravity-cli.md) — measured MCP identity, hook payloads, injection forms and Stop re-entry on agy 1.5.0; backs ADR 0012.
