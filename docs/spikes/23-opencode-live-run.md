# Spike 23: OpenCode live run (slice 9)

Date: 2026-11-23. Issue #273 (parent #272). Docs only; no product code. Results of the plan's section 8 live run (`docs/plans/opencode-harness.md`), plus M3 and the D15 `next_turn` no-turn check.

Terms. **L1 to L13**: the numbered live scenarios in the plan's section 8 table. **M3**: the pre-build measurement of whether OpenCode v2 prompts before a CrossTalk MCP tool runs. **bound**: an OpenCode session that has called a CrossTalk tool, so the shim knows its session id. **service**: the shared background server (`opencode serve --service`) whose url, pid and password are in `service.json`. **inbox**: `GET /api/session/<id>/inbox`, messages admitted but not yet given to the model. **Result**: pass, fail (with the issue number), partial, or pass (reduced), a pass with the gap named in the Actual column.

## Setup

OpenCode 2.4.1 (Homebrew), model `openrouter/qwen/qwen3.8-27b`, the CrossTalk binary built from the worktree (version string 0.2.6, with the OpenCode harness). L1, L1b and L12 ran in scratch `HOME` and `XDG_*` directories. The live steps ran against the operator's real OpenCode directories (operator ruling in the kickoff: OpenCode is unused on this Mac): the global `opencode.json` was absent before and was created with the model and the `crosstalk` entry, then removed at the end; the service was started by hand and stopped at the end.

Peers. The Claude session that ran this slice uses the released 0.2.6 shim, which has no OpenCode harness, so it cannot see or address an OpenCode session; it could only answer an ask that an OpenCode session sent it (L6). The Codex peer was the standing reviewer session, which has the same limit, and it answered one ask. The senders for L3, L4, L5, L7 and L11 were therefore a second OpenCode session (`s9-beta`) bound by one model call. A later run with a build that has the OpenCode harness in the Claude and Codex shims can close the reduced-scope gaps.

Budget: stated before the first model call as 20 prompts that start a turn, stop at 40. Used: 22 (each prompt is one to three OpenRouter requests). Session cost shown by the service: about 0.07 USD for the two sessions that ran model turns.

## Results

| Step | Expected | Actual | Result | Model | OpenCode | Date | Issue |
|---|---|---|---|---|---|---|---|
| L1 | Other entries and comments intact; K2, K5, K6, K13 pass | `install opencode --yes` on a scratch `opencode.json` with a `//` comment, a `/* */` comment, a trailing comma and another server added `mcp.servers.crosstalk`; both comments and the comma intact (the neighbouring entry was reformatted to multi-line by the OpenCode CLI, as M4 found); K2, K5, K6, K13 PASS (K13: service not running) | pass | none | 2.4.1 | 2026-11-23 |  |
| L1b | Commented: refuses, prints the lines, changes nothing. Comment-free: removes only CrossTalk's entry, either shape | Commented: exit 1, printed the 7 lines from line 14, file byte-identical (`cmp`). Comment-free, v2 shape (`mcp.servers`) and v1 shape (`mcp.<name>`): each removed only the `crosstalk` entry and left the other server and keys | pass | none | 2.4.1 | 2026-11-23 |  |
| L2 | Unbound row from the session title, `crosstalk` flag flips after the first call | A new session `s9-alpha` listed with `harness: opencode`, `crosstalk: false`; after one prompt whose model called `tools.crosstalk.list_agents` the same `agent_id` listed `crosstalk: true`. Reduced scope: the rows were read through the worktree-built shim (a fake MCP client), because the Claude session's own shim is the released 0.2.6, which has no OpenCode harness | pass (reduced) | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L3 | Sender sees `accepted`; prompt carries the preamble; model answers; no `system` or synthetic | A bound OpenCode session `s9-beta` sent an inform to `s9-alpha`: the stored user message is the preamble line plus one JSON object (`notice`, `from`, `from_harness`, `sender_verified: true`, `message_id`, `in_reply_to`, `kind`, `parts`), keys `id,text,time,type` only; the model answered. Reduced scope: the sender is an OpenCode session, not Claude. A `crosstalk send` (unverified sender) gets `mailboxed` and the fixed wake text "A CrossTalk message is waiting: call receive_message", and the model pulled it with `receive_message` (the D15 path) | pass (reduced) | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L4 | Arrives at the next model call, `accepted` | `s9-alpha` ran `sleep 40` in its shell tool; `s9-beta` sent an inform 11 s in. The user message was stored right after the tool call completed and before the next assistant request; the same turn finished; no second turn, empty inbox. Reduced scope: the sender is an OpenCode session, not Codex | pass (reduced) | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L5 | Task completed with the reply text, right `in_reply_to` | `s9-beta` asked `s9-alpha` (2+2); alpha was woken with `kind: ask`, called `reply`; beta was woken with an inform from alpha whose `in_reply_to` equals the ask's `message_id` and whose text is `4`. Reduced scope: both ends are OpenCode; a Claude or Codex asker cannot address an OpenCode session through the released 0.2.6 shims | pass (reduced) | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L6 | Round trips complete | `s9-alpha` asked the Codex reviewer session: it was accepted, the Codex session replied (declining the content, its role is reviews only) and the reply reached alpha with `in_reply_to` equal to the ask id, sender verified. `s9-alpha` asked the Claude worker session: it was answered `PONGCLAUDE` by `reply`, which reached alpha with the right `in_reply_to` | pass (transport); Codex answer was a refusal | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L7 | `next_turn` stored without a turn; `immediate` wakes | See the `next_turn` row for the no-turn half. An `immediate` inform then started a turn on the idle session, the stored `next_turn` message and the new one were both in the turn, the inbox emptied | pass | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L8 | One process, two agents, no cross-bind, own mail only | `s9-alpha` and `s9-beta` share one working directory; one `crosstalk mcp` process (child of the service) serves both; two distinct `agent_id`s; no message appeared in the wrong session over the run | pass | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L9 | `list_agents` drops an archived or deleted session within a sweep; a send after the drop gets the no-such-agent result; a send in the window gets `unreachable` | An unbound session deleted through the API left `list_agents` at once and a send named no such agent. A bound session archived and then deleted stayed in `list_agents` (`reachable: false`) for the whole observation, minutes, and a send to it answered `mailboxed`. Cause: `listSessions` treats the list as incomplete whenever the response has a field besides `data`, and 2.4.1 always adds `cursor`, so `Superseded` is always false | fail | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 | #316 |
| L10 | `unreachable` while down, works after with no CrossTalk restart | Stopping the service removed `service.json` and ended its `crosstalk mcp` child; a send then answered no such agent (the descriptor was pruned), not `unreachable`. After the restart the password was unchanged (the new-password case did not occur in 2.4.1), sessions listed `crosstalk: false` until one call per session rebound them, and a send then reached the session. No CrossTalk install or restart step was needed | partial | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L11 | Model-visible text keeps the real notice; outer fields intact | A verified sender's body held a fake closing delimiter, a fake notice and a fake JSON object; the stored prompt is the preamble plus one JSON object that parses, with the real notice, `from: s9-beta`, `sender_verified: true` and `kind: inform` set by CrossTalk, the hostile text only inside `parts[0].text`; the model reported it and did not act. Reduced scope: the sender is an OpenCode session, not Claude | pass (reduced) | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| L12 | `install opencode` and `--check` say v1 is unsupported; no config changed | With a stand-in `opencode` printing `1.4.0`: `install` exit 1, "OpenCode 1.x found: unsupported, see ADR 0032; nothing was written", config byte-identical (the binary copy to the bin directory had already run). `--check` printed no 1.x or unsupported text: K2, K5, K6 FAIL (no entry), K13 PASS (service not running), because K13 reads only `service.json` | fail (`--check` half) | none | 2.4.1 | 2026-11-23 | #317 |
| L13 | Forced dispatch: `compare-opencode` and the live legs pass; a bump PR proposes the version | Not dispatched here. Run 45288605132 (forced, 2026-11-23, head `d2d77db`, the #312 fix included, opencode 2.4.1): every job succeeded (`compare-opencode-linux`, `compare-opencode-macos`, `harness-opencode-linux`, `harness-opencode-macos`, both builds, `report`, `Open issues for failures`, `Record passing versions`); the bump PR #315 recorded 2.4.1 and is merged | pass (cited) | the compat run's own OpenRouter model | 2.4.1 | 2026-11-23 |  |
| M3 | Prompts or not; which key silences it | No prompt. With no `permission` key in `opencode.json`, the model's call of `tools.crosstalk.list_agents` through the `execute` tool completed; `GET /api/session/<id>/form` (the pending-question list) stayed empty, polled every 3 s for 24 s; no stall. Signal used: the tool part's `status: completed`, the empty form list and `/active` returning to `{}` | pass: v2 does not prompt by default | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |
| `next_turn` no-turn (D15) | No turn starts; the message is stored; request count unchanged | A bound sender sent `delivery: next_turn` to an idle session: sender state `queued`; afterwards the session's message count (35), assistant count (16), cost and token totals were identical, `/active` was `{}`, and `GET .../inbox` held the CrossTalk prompt (delivery `queue`), not `/message`, as spike 22 Q3 measured. The `immediate` send then started the turn (L7). #306 has not regressed | pass | `openrouter/qwen/qwen3.8-27b` | 2.4.1 | 2026-11-23 |  |

## Commands (key redacted)

```
S=<scratch>; go build -o $S/bin/crosstalk ./cmd/crosstalk
# L1, L1b, L12 (scratch profile)
export HOME=$S/l1/home XDG_CONFIG_HOME=$S/l1/cfg XDG_DATA_HOME=$S/l1/data XDG_STATE_HOME=$S/l1/state XDG_CACHE_HOME=$S/l1/cache OPENCODE_DISABLE_AUTOUPDATE=1 OPENCODE_DISABLE_MODELS_FETCH=1 OPENCODE_DISABLE_CLAUDE_CODE=1
$S/bin/crosstalk install opencode --yes; $S/bin/crosstalk install opencode --check
$S/bin/crosstalk uninstall opencode --yes          # commented config: refuses; comment-free copies: removes the entry
PATH=$S/fakebin:/usr/bin:/bin $S/bin/crosstalk install opencode --yes   # fakebin/opencode prints 1.4.0 for --version
# live (real HOME, key only in the subshell environment)
printf '{"model":"openrouter/qwen/qwen3.8-27b"}
' > ~/.config/opencode/opencode.json
$S/bin/crosstalk install opencode --bin-dir $S/bin --yes
( set -a; . ~/.config/crosstalk-m1.env; set +a; export OPENCODE_DISABLE_AUTOUPDATE=1 OPENCODE_DISABLE_MODELS_FETCH=1 OPENCODE_DISABLE_CLAUDE_CODE=1
  nohup opencode serve --service > service.log 2>&1 & )     # OPENROUTER_API_KEY comes from the env file
# M6 check: service.json pid equals the listener pid on 49374 before any send
lsof -nP -iTCP:49374 -sTCP:LISTEN; jq .pid ~/.local/state/opencode/service.json
# every API call: the password goes through curl --config - on stdin, never a command line
printf 'user = "opencode:%s"
' "$PW" | curl -s --config - -X POST -H 'content-type: application/json' --data '{"title":"s9-alpha"}' "$URL/api/session"
... --data '{"text":"<one sentence>","delivery":"steer"}' "$URL/api/session/$SID/prompt"
... "$URL/api/session/active" ; "$URL/api/session/$SID/message" ; "$URL/api/session/$SID/inbox" ; "$URL/api/session/$SID/form"
# archive and delete (L9)
... -X PATCH --data '{"archived":true}' "$URL/api/session/$SID" ; ... -X DELETE "$URL/api/session/$SID"
```

`list_agents` rows for OpenCode sessions were read by driving the worktree shim as a plain MCP client over stdio (an `initialize` then a `tools/call`); `crosstalk send <name> <text>` was the unverified-sender path.

## Side findings

- A `crosstalk send` only addresses a bound session; an unbound OpenCode session is in `list_agents` (`crosstalk: false`) but `send` answers no CrossTalk agent of that name.
- A reply to an inform is delivered to the sender as a raw prompt and starts the sender's own turn; a ping-pong between two sessions that always reply is possible (the nudge at 30 sends per peer is the only brake, ADR 0014).
- The service wrote a copy of `service.json` under `$XDG_CONFIG_HOME/opencode/` as well as under `$XDG_STATE_HOME/opencode/`.
