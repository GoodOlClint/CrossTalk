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
- [16 remote gateway on real machines (S2)](16-live-real-machines.md) — partial: VM legs pass on a same-host stand-in; every Mac handshake stalled, most likely a network filter waiting for an operator decision; pending the operator. Driver in `16-live-real-machines/`.
- [17 CI two-host job (S6)](17-ci-two-host.md) — v0.2.0 spike: two Docker containers on the Ubuntu runner pair through the real `remote pair` / `remote join` CLI with Fake-backed agents and deliver one inform in under 1 s and one ask and reply in about 4.3 s (limit 180 s), 4 of 4 CI runs; PASS. Code later promoted to `internal/remotee2e/` and `scripts/remote-e2e/` (R10).
- [18 remote send latency and settle (S5)](18-live-latency-settle.md) — partial: stand-in 10 of 10 `accepted`, median 112 ms; the Wi-Fi series is pending; keep `RemoteSettle` at 5 s.
- [19 login unit (S7)](19-live-login-unit.md) — partial: VM linger and user unit pass; reboot and Mac legs pending the operator; `enable` legs blocked-on-R11; `remote status` does not report `service running, port not bound` for a foreign listener. Units in `19-live-login-unit/`.
- [21 OpenCode](21-opencode.md) — opencode-ai 1.19.6 and @opencode/cli 2.4.0 measured with a fake local model: an API prompt reaches the session the TUI shows, idle or mid-turn, as a user message; v2 sends the session id in `_meta` and advertises its server in `service.json`, v1 sends neither and opens no socket without `--port`; Kilo follows v1 (source read).
- [22 OpenCode pre-build](22-opencode-prebuild.md) — slice 0 of the OpenCode harness (#273): M6 delivery details and M4 install mechanics on OpenCode 2.4.1 (`queue` with `resume: false` stores without a turn; an archived session still accepts a prompt; `opencode mcp add` keeps comments). M1's results are in the plan.
- [23 OpenCode live run](23-opencode-live-run.md) — slice 9: the section 8 live run on OpenCode 2.4.1. Failed first on retirement (#316) and `--check` (#317), both fixed and re-run; the in-window retirement send stays unobserved.
- [24 Pi pre-build](24-pi-prebuild.md) — slice 0 of the Pi harness (#275): E1 to E10 re-run and M1 to M6 on Pi 1.6.0. The server's parent is the Pi process and the shim-hosted socket proves it by peer pid (macOS and a Linux container); `direct` tools are first-class but Pi waits at most 10 s for them; extensions get per-message admission events for `triggerTurn: true` but not for `steer` + `triggerTurn:false` and cannot see an asynchronous send rejection, which changes the D7 ack; a busy `steer` + `triggerTurn:false` message is flushed at the next `turn_end`; an uncaught extension error crashes Pi; M1 (one model) did not follow the permission-laundering request, so D6 stands; spend $0.0074. M5 reduced (real-shim timing carried to slice 5); M6 reduced (Linux arm64 container).
- [25 Pi live run](25-pi-live-run.md) — slice 9 of the Pi harness (#275): the section 8 live run on Pi 1.6.0 with scratch Claude, Codex and OpenCode senders. Delivery, identity, refresh and K11 work; Pi's own `list_agents` was rejected by argument validation with the model used (#347); the compat dispatch is pending.
