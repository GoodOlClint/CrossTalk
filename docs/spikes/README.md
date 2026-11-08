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
- [13 remote SDK seam](13-remote-sdk-seam.md) — S1: a pinned-mTLS gateway in front of an a2a-go v2.7.0 loopback server keeps the host fingerprint from the connection, refuses malformed agent fields before the receiver chain and gives a remote primary no `command`; PASS, with R4 findings. Code in `13-remote-sdk-seam/`.
- [14 remote pairing](14-remote-pairing.md) — S3: wrong, expired, second and concurrent redemptions write nothing or enroll exactly one; an open and a resumed connection are refused after `unpair`; PASS. Code in `14-remote-pairing/`.
- [15 remote reply across a disconnect (S4)](15-remote-reply-disconnect.md) — v0.2.0 spike: kill the target gateway after injection and before the response; the asker shows `unknown`, the retry injects once, the reply reaches the original asker and never a name reuser; backs plan section 8 and slice R8.
- [17 CI two-host job (S6)](17-ci-two-host.md) — v0.2.0 spike: two Docker containers on the Ubuntu runner pair through the real `remote pair` / `remote join` CLI with Fake-backed agents and deliver one inform in under 1 s, 3 of 3 CI runs; the ask and reply leg is blocked on R8 (PR #77). Code in `17-ci-two-host/`.
