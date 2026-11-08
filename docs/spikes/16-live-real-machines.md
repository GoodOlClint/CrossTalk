# Spike 16 — remote gateway on real machines (v0.2.0 S2)

- **Date:** 2026-11-08
- **Status:** PARTIAL. The Linux VM legs ran. Every leg that touches the Mac is PENDING-OPERATOR: on the Mac every TLS handshake stalled, most likely held by a third-party network filter that waits for an operator decision (below).
- **Plan:** `docs/plans/release-0.2.0.md` section 10 (S2), sections 3.2 and 7; gates R5 (live) and R12.
- **Build under test:** main `3d7f55b` (R1-R9 merged, including R8) for the VM; the Mac legs used `6db91e4` and were not repeated. Both builds are ad-hoc signed `go build` outputs of the `crosstalk` binary, **not** the signed release binary the plan row names.
- **Code:** `docs/spikes/16-live-real-machines/driver/` (throwaway `package main`, imports production packages, edits none). It runs the production shim, wire and MCP server over `adapter.Fake` (a fake Codex harness), so a host with no Claude or Codex login can send, receive and reply.

## Question

Does `remote serve` on the operator's Mac and on a Linux VM on the same LAN accept pinned connections, can each host dial the other, and what does macOS put in the way?

## Method

1. Build the `crosstalk` binary for darwin/arm64 and linux/amd64; run `remote config --bind <LAN address>` on each host.
2. `remote serve` in the foreground on each host, then `remote pair` on one and `remote join` on the other over the direct LAN addresses (no tunnel).
3. Stand-in for the missing Mac path (labelled as such everywhere): two gateways on the VM, each with its own `HOME`, each bound to the VM's LAN address on a different port (7745 and 7746), paired through the real CLI. Same kernel and same address, so no network latency or middlebox is measured.
4. The driver registers a fake agent, then `list_agents`, `send_message` and `ask` run through the production tools.

## Pass lines (plan row S2, one per clause) and results

Terms: **gateway** is the `remote serve` process; **pairing** is the one-time `remote pair` / `remote join` token exchange; **PENDING-OPERATOR** means the leg needs a decision or presence the operator has not given yet.

| # | Pass line clause | Result | Evidence |
|---|---|---|---|
| 1 | The signed binary | NOT RUN | The release binary is built and notarized in CI on a tag; only ad-hoc builds were used. Same code, different signature, which matters for the Mac filter below (a rule is per path and signature) |
| 2 | `remote serve` in the foreground accepts, VM | PASS | `gateway listening on <ip redacted>:7745`; a Go TLS probe from the VM reached it (handshake 0.6 ms) |
| 3 | `remote serve` in the foreground accepts, Mac | FAIL (stalled), PENDING-OPERATOR | The gateway binds and prints its start line, but no ClientHello arrives: the goroutine dump shows the handshake goroutine blocked in `readHandshakeBytes` while a client waits for the ServerHello. A VM client times out at 5 s; the Mac's own client does the same to its own LAN address |
| 4 | `remote serve` as a login unit accepts, VM | PASS | `status` against the unit-run gateway after `systemctl --user enable --now`: `gateway: running`, peer `ok` (spike 19) |
| 5 | `remote serve` as a login unit accepts, Mac | NOT RUN | Mac networking blocked; launchd template is ready (spike 19) |
| 6 | Pairing over the direct LAN, VM to VM stand-in | PASS | `paired with vma (...) at <ip redacted>:7745` / `paired with vmb (...)` |
| 7 | Pairing over the direct LAN, Mac and VM, either direction | FAIL (stalled), PENDING-OPERATOR | Mac pairs, VM joins: `Post "https://<ip redacted>:7745/v1/pair": EOF` after about 7 s. VM pairs, Mac joins: the same error. Nothing was written on either side |
| 8 | A shim dials out and receives, both directions, VM stand-in | PASS | `list_agents` on one gateway shows the other's agent as `bob@vmb`, `messageable: true`, no `cwd`, `pid` or endpoint; 10 sends and 1 ask all `accepted`; the ask's reply arrived (below) |
| 9 | A shim launched by Claude, and by Codex, dials out | NOT RUN | No harness login on the VM (operator ruling: later); on the Mac the path is blocked. The fake harness stands in for the harness side only |
| 10 | Both directions between Mac and VM | PENDING-OPERATOR | Blocked by row 3 and 7 |
| 11 | Over the tailnet (100.x) | NOT RUN | The Mac has no 100.64.0.0/10 interface (a WireGuard profile exists but is disconnected); no install attempted |
| 12 | No System Settings change by the worker | PASS | None made; no firewall, Local Network or filter setting touched |
| 13 | The exact macOS Local Network prompt or firewall setting is recorded | PARTIAL | No macOS Local Network dialog was identified and the screen cannot be read from the session (`screencapture` fails: could not create image from display). Recorded settings: Application Firewall on, stealth mode off, built-in and downloaded signed apps auto-allowed, the test binary reported as permitted. The blocker is not the Application Firewall |
| 14 | Sleep and wake, pairing survives | NOT RUN | Operator ruling 2026-11-08: a manual test when the operator is home |
| 15 | Ask and reply crossing hosts (R8 line, added) | PASS (stand-in) | Ask from `alice@vma` to `bob@vmb`; the fake agent replied; the reply arrived as an inform with `in_reply_to` set and text `spike pong`. Not shown across a Mac |

## What stalled on the Mac, and how it was narrowed

Facts, in the order they were established:

1. TCP connect to the Mac gateway succeeds from the VM, but no TLS handshake completes in either direction between the Mac and the VM, and a Go TLS client on the Mac hangs against the VM gateway too. The same probe built for Linux handshakes with the VM gateway in 0.6 ms.
2. A plain Python TCP server on the Mac's LAN address received nothing from a client on the same Mac (10 bytes up to 1800 bytes), while 127.0.0.1 delivered. So the stall is below the application and not specific to the CrossTalk binary. (The 127.0.0.1 control matters: the gateway only listens on the LAN address, so every gateway test has to cross the filtered path.)
3. Not a post-quantum key-share size effect: a client restricted to X25519 (a small ClientHello) stalled the same way.
4. The Mac runs Little Snitch (a network extension, version 6.5, active) and Malwarebytes. Little Snitch holds a new binary's flows until the operator answers an alert. This is the most likely cause. It is **not confirmed**: the alert text could not be read and no rule was changed. If it is confirmed, the exact alert text and the rule needed (incoming TCP 7745 for the gateway, outgoing TCP to the peer's 7745) belong in setup.md.

Per the stop-and-report rule nothing was clicked, allowed or worked around. The gateway binary was copied to a stable path so one rule can apply, and not rebuilt afterwards. The gateway was stopped when the work paused.

## Surprises and findings for later slices

1. **`remote unpair` does not exist in this build.** The plan (section 4.1, spike S3, slice R3) names it; the shipped `remote` commands are `config`, `serve`, `pair`, `join` and `status`. Removing a pairing today means editing `peers.json`. This is a gap against the plan; R3's list or a follow-up slice must close it, or the plan must say it is deferred.
2. **A stalled handshake is silent.** Neither `remote serve` (no log line) nor `remote join` (a bare `EOF` after about 7 s) says that a handshake never completed. A firewall or filter that holds traffic looks like a protocol bug. R12 should add a troubleshooting entry, and `join` should name the handshake timeout and the likely cause.
3. **`list_agents` shows the fake agent as `reachable: false` with an empty status** while `messageable: true`. The gateway process reports reachability from its own harness check, and the fake Codex thread exists only inside the driver, so this is an artifact of the stand-in, not a defect. A real Claude or Codex session should be checked in the pending live run.
4. **`remote join` and `remote pair` print no hint about an unreachable gateway before waiting.** `pair` waits up to 10 minutes with no sign the peer cannot reach it.

## What the later slices must change

- R12 docs: a "firewall and network filters" section; state that a per-application firewall that prompts on new binaries will hold the first handshake, that a rule is per path and signature, and that `crosstalk install` (stable path) helps; give the exact prompt text after the operator session.
- R12 live run: repeat rows 3, 5, 7, 9, 10, 14 with the signed release binary after the filter decision.
- R3 follow-up: `remote unpair`, or a plan amendment.
- Hand-off for the pending operator session: the filter alert, Mac sleep and wake, VM Claude and Codex logins, then rerun this spike from row 3.
