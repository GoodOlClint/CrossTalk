# Spike 25: Pi live run (slice 9)

Date: 2026-11-25. Issue #275 (parent #272). Docs only; no product code. Results of the plan's section 8 live run (`docs/plans/pi-harness.md`), plus the open M items from slice 0 and the K11 match against a real Pi process.

Terms. **L1 to L13**: the live scenarios in the plan's section 8 table. **M4, M5, M6**: pre-build measurements the plan left open (section 7). **K2 to K14**: checks of `crosstalk install --check`. **Shim**: the `crosstalk mcp` process. **TUI**: Pi's terminal screen. **Turn**: one agent run started by a typed prompt or a delivered message; it can make several model requests. **Result**: pass, fail, partial, pass (reduced) with the gap named, or not run. A row that delivers to or from a Pi session passes only when the session file, the TUI and the journals agree; a row that lacks one of the three is pass (reduced). L1, L1b, L12, M4 and K11 test the installer, the registry and the process table, and are judged on those: installer and check output, file hashes, descriptors and `ps`.

## Setup

Pi 1.6.0 (Homebrew), macOS arm64, CrossTalk built from main at `bb4a37d` (version string 0.3.0), model `openrouter/qwen/qwen3.8-27b` for every model turn. Every run used a scratch `HOME`, `PI_CODING_AGENT_DIR`, `CODEX_HOME` and `XDG_*` directories and `PI_TELEMETRY=0`; the offline rows (L1, L1b, L12) used separate profiles with `PI_OFFLINE=1` and no key. The real profiles and installed binary were not touched.

Senders. All three are **scratch-profile sessions on the same OpenRouter key and model, not the operator's own sessions**: a Claude Code 2.1.332 session run headless (`claude -p` with stream-json input, kept open), a Codex 0.172.0 session on its own managed daemon, and an OpenCode 2.4.1 service session. Each ran the scratch CrossTalk build, installed with `crosstalk install <harness> --yes`.

Budget: stated before the first model run as 20 turns, stop line 40, counting every harness on the key. Used: 34 (Pi 15, Claude 15, Codex 2, OpenCode 2). The count is over the plan because each reply that reaches a session starts a turn there. Spend: **0.108 USD** (OpenRouter's usage counter for the key, 0.0086 before and 0.1165 after, of a 5 USD cap). The last model turn ended on 2026-11-25, before the key's expiry.

The key was read from a file inside the wrapper that starts each scratch process and passed by environment. It was grepped for, with `sk-` tokens, over the diff, this file and the scratch logs, session files and journals before the commit.

## Results

| Step | Expected | Actual | Result | Model | Pi | Date | Issue |
|---|---|---|---|---|---|---|---|
| L1 | Other files untouched; K2, K5, K6, K14 pass | `install pi --yes` into an agent dir holding another extension and an `mcp.json` with another server: both files byte-identical afterwards (SHA-256); K2, K5, K6, K14 PASS | pass | none | 1.6.0 | 2026-11-25 | |
| L1b | Own file removed; a foreign `extensions/crosstalk/index.ts` reported and kept | `uninstall pi` removed the extension and its directory and left the other extension and `mcp.json` byte-identical. With a foreign `index.ts` in place, `uninstall` printed that it is kept and `install` refused (exit 1); the file stayed byte-identical both times | pass | none | 1.6.0 | 2026-11-25 | |
| L2 | Row with `reachable: true`, a status, the `--name` name and `crosstalk: true` before any model tool call; the model's own `list_agents` recorded; row gone after quit | The Claude sender's `list_agents`, before Pi had any turn, listed the row: `harness: pi`, `status: idle`, `reachable: true`, `crosstalk: true`, name from `--name`. After quit no Pi row was listed and the socket directory was empty. Pi's own call: 4 of 4 `mcp__crosstalk__list_agents` calls (two prompts at default thinking, one session with `--thinking off`) were rejected by Pi's argument validation because the model added a field (`tool`, `to`), so none reached CrossTalk and no result was obtained | partial | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | #347 |
| L3 | Sender `accepted`; TUI shows `[crosstalk]` and the envelope; model answers | Claude `send_message` to the idle Pi: `accepted`; the TUI showed `[crosstalk]`, the preamble and the JSON object with `from_harness: claude`, `sender_verified: true`; the session file holds the custom message; both journals record `accepted`; the model called `reply` (`accepted`) and the Claude session received it | pass | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L4 | Arrives at the next turn boundary of the same run, after the tool batch; sender `accepted` or `unknown` | Pi ran `sleep 90`; the Codex sender sent 15 s in and got `unknown` with reason `pending` (the receiver recorded `unknown` 10 s after the send). The custom message was written 1 ms after the tool result, before the next assistant message, in the same run | pass (reduced: TUI not captured) | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L5 | Claude `ask`, Pi `reply`, right `in_reply_to` | The ask was `accepted`; Pi called `reply` with `4`; the asker's journal holds a receipt whose `in_reply_to` is the ask id, and the Claude session printed the answer | pass (reduced: TUI not captured) | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L6 | Round trips to Codex, Claude and OpenCode complete | One Pi prompt sent three `ask`s (by agent_id), each `accepted`; three replies arrived as custom messages (`PONG`, `sender_verified: true`, `from_harness` codex, opencode and claude), each with the matching `in_reply_to`; the asker's journal holds three receipts | pass (reduced: TUI not captured) | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L7 | Idle `next_turn`: shown, no turn, sender `queued`; a later `immediate` starts a turn. Busy `next_turn`: no admission event, flushes at the running turn's end, sender `unknown` without evidence | Idle: sender `queued`; the custom message is in the session file and on the TUI with no assistant message after it 30 s later, status file `idle`. The later `immediate` was `accepted` and started a turn. Busy (`sleep 40`): sender `unknown` with reason `pending`; the message was written just after the tool result and before the last assistant message of the same run. The receiver's journal recorded `unknown` 10 s after the send and nothing later; whether Pi emitted an admission event was not observed (spike 24 M3 measured none) | pass (reduced: busy half has no captured TUI and no event trace) | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L8 | Old agent gone within a sweep, new session listed, mail to the old id gets no-such-agent or unreachable | `/new` (TUI "New session started"): same Pi pid, new shim, new `agent_id` and session ref, default name. `list_agents` showed only the new Pi row, and `send_message` to the old `agent_id`, 14 s after `/new`, returned `unknown` with reason `no_such_agent` (sender's journal). No session file records a send that finds no session; the time at which the old row left was not measured | pass (reduced: no session-file record) | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L9 | One reconnect, same session, next send works, no Pi restart | The binary was replaced by rename with a different build. The shim kept its pid and start time and ran the new file's inode; `agent_id` and session ref unchanged; its socket handles to the Pi socket were new. The next Claude send was `accepted`, delivered into the same session, and answered. Pi was not restarted | pass (reduced: TUI not captured; the number of reconnects was not counted) | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L10 | `mailboxed`; only the fixed wake in the session; the model pulls the body | `crosstalk send` returned `mailboxed`. The session got one custom message, exactly "A CrossTalk message is waiting: call receive_message"; the body's marker is in no custom or user entry. The model called `receive_message` and got the body with `from: unverified` and the information notice | pass (reduced: TUI not captured) | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L11 | Outer fields intact, real notice kept | A Claude sender sent text holding closing braces, a closing tag, a fake notice and a fake JSON object. The session entry is the preamble plus one JSON object that parses, with the real Claude notice, `from_harness: claude`, `sender_verified: true`, `kind: inform`, one part, and the hostile text only inside `parts[0].text` | pass (reduced: TUI not captured) | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | |
| L12 | With a 2.x `pi` and a different binary installed: refuses, writes no Pi file; K14 names it | A stand-in `pi` printing `2.0.0`: `install pi` exit 1, "major 2 (tested majors [1]); no Pi file was written"; the agent dir stayed empty. The shared binary step had already replaced the different binary. `--check` exit 1: K2, K5, K6 FAIL, K14 WARN naming major 2 | pass | none | 2.0.0 (stand-in) | 2026-11-25 | |
| L13 | Forced compat dispatch: both Pi legs pass, a bump PR records the version | No `harness-compat` run with the Pi legs exists (the latest run predates them). Not triggered | not run: pending the operator's dispatch | | | | |
| M5 | The Pi model calls `list_agents` through the real shim; first-prompt tools present | In both Pi sessions whose first prompt asked for it, the model emitted a `mcp__crosstalk__list_agents` call on that prompt, so the tools were present. The call never reached the shim (see L2). `receive_message`, `reply` and `ask` calls did. The shim's start time was not timed | partial | `qwen3.8-27b` | 1.6.0 | 2026-11-25 | #347 |
| M4 | The session ref a real Pi reports; two processes on one session file | A session with a session file registered `<session id>@<8 hex>`; a `--no-session` Pi registered the bare session id, with no model turn (registry only; it has no session file). Two Pi processes on one session file were not run through CrossTalk | pass (reduced: two processes on one session file not run) | none | 1.6.0 | 2026-11-25 | |
| M6 | E3, E4 and E6 on the x64 runner | No dispatch (see L13) | not run: pending the operator's dispatch | | | | |
| K11, `IsPiHost` | A real Pi is recognised as a host and grouped by K11 | `ps -o command=` and `ps -axo command=` read `pi` for the Pi process, and registration, which requires `IsPiHost`, succeeded. A second Pi with `CROSSTALK_NO_AUTO_REFRESH=1` in its environment kept its old binary after the replacement: K11 listed that one shim under "Pi sessions" with "run /reload in each Pi session". After `/reload` it ran the new binary, kept its `agent_id` and left the group | pass | none | 1.6.0 | 2026-11-25 | |

`qwen3.8-27b` is `openrouter/qwen/qwen3.8-27b`.

## Reduced and not run

- **L2, M5 (partial):** Pi's own `list_agents` result was never obtained (#347). M5's start-time half was not timed.
- **L4, L5, L6, L7 (busy half), L9, L10, L11:** the Pi screen was not captured for these rows; they rest on the session file and the journals.
- **L7 (busy half):** whether Pi emitted an admission event was not observed.
- **L8:** no session-file record, and the time the old row left was not measured.
- **L9:** the number of reconnects was not counted.
- **M4:** two processes on one session file were not run through CrossTalk.
- **L13, M6:** pending the operator's compat dispatch. With them, the two slice 7 threads (util-linux `script` with a real Pi, and the live leg's missing check of the asker's `list_agents`) stay open.
- **OpenCode parents in K11** were not produced in this run; only the Pi group was.
- One model, one sample per row. Notice effectiveness is not measured.

## Side findings

- K11 lists every `crosstalk mcp` process on the host that does not run the profile's binary, so a scratch profile's `--check` also names the real sessions' shims.
- A headless Claude session with stream-json input held open did receive replies.

## Commands (key redacted)

```
# $S is a short symlink to a scratch directory (macOS socket paths are limited to 104 bytes); $ENV_FILE holds the key
go build -o $S/bin/crosstalk ./cmd/crosstalk
# every scratch process: env -i HOME=$S/h PI_CODING_AGENT_DIR=$S/h/.pi/agent CODEX_HOME=$S/h/.codex XDG_*=... PI_TELEMETRY=0 ...
# model processes also: OPENROUTER_API_KEY, read from $ENV_FILE inside the wrapper; Claude adds ANTHROPIC_BASE_URL=https://openrouter.ai/api and ANTHROPIC_AUTH_TOKEN
$S/bin/crosstalk install pi --yes ; ... install claude --yes ; ... install codex --yes ; ... install opencode --yes
tmux -L <private> new-session -d -s <session> "<wrapper> pi --model openrouter/qwen/qwen3.8-27b --name <name>"
claude -p -n <name> --input-format stream-json --output-format stream-json --verbose     # prompts written to its stdin fifo
codex app-server daemon start ; codex -a never -s workspace-write
opencode serve --service     # session and prompt through the service API, password on stdin through curl --config -
# L9: build a different binary, then rename it over $S/h/.local/bin/crosstalk
# L10: crosstalk send <name> "<text>"
```

## Issues filed

- #347: Pi: a model that adds a field to a no-argument call cannot use `list_agents` (L2, M5).
