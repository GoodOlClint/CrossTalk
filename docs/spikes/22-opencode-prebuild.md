# Spike 22: OpenCode pre-build measurements

Date: 2026-11-23. Issue #273 (parent #272). Docs only; no product code. This file is the home of the plan's slice 0 measurements (`docs/plans/opencode-harness.md` section 7). M1's results currently live only in the plan; M6's are here.

## M6: delivery details

**Setup.** OpenCode 2.4.1 (Homebrew install, the installed binary, not an npm install), model `openrouter/qwen/qwen3.8-27b` set through `opencode.json` in the scratch config (v2 has no `-m` flag). Scratch `HOME` and `XDG_*` directories under `/tmp`, `OPENCODE_DISABLE_AUTOUPDATE=1`, `OPENCODE_DISABLE_MODELS_FETCH=1`, `OPENCODE_DISABLE_CLAUDE_CODE=1`. The scratch OpenRouter key was read from the environment only; Basic auth went through `curl --config -` on stdin. Sessions were created with `POST /api/session {"title":…}`, which works, so no typed first prompt is needed. TUIs ran in detached tmux sessions and their screens were read with `tmux capture-pane`. One run per cell: one sample, not a rate. Spend: $0.020 of the scratch key's cap. At the end the scratch service, every TUI and every tmux session were stopped and port 49374 was confirmed free.

Terms. **Service**: the shared background server (`opencode serve --service`) that a plain `opencode` TUI starts on first use and reuses; `service.json` is its url, pid and password file. **steer** and **queue**: the `delivery` values of `POST /api/session/<id>/prompt`. **inbox**: `GET /api/session/<id>/inbox`, the messages admitted but not yet given to the model. **resume**: an optional boolean in the prompt body; `false` was the candidate no-wake switch.

| # | Question | Observed |
|---|---|---|
| 1 | Listener ownership | Before any request, `service.json` pid 64414 equalled the `lsof -nP -iTCP:49374 -sTCP:LISTEN` pid 64414; the service is `opencode serve --service`, parent pid 1, and survives its TUI being killed. With port 49374 held by an unrelated process, `opencode serve --service` failed (`Failed to start server. Is port 49374 in use?`, exit 1) and wrote no `service.json`. A second `opencode serve --service` started while the first runs exits 0 silently and the first keeps the port and the file unchanged; a TUI started then reuses it. `service.json` is deleted when the service is stopped with SIGTERM. Not measured: a TUI started while a foreign process holds the port (the `serve` case above already fails) |
| 2 | `queue` on an idle session | Starts a turn at once: 200, `GET /api/session/active` showed `running` within 2 s, the assistant answered OK |
| 3 | `resume: false` | Idle session, `steer` + `resume:false`: 200 admitted, no turn (`active` stayed `{}` for 6 s), `GET .../message` had no user message, the text sat in `GET .../inbox`. Idle session, `queue` + `resume:false`: same (no turn, inbox only, message list empty). It joins the next turn: after a later plain prompt the model's turn included both user messages and the inbox emptied. So it is stored, not dropped, and it waits for any later prompt to that session |
| 4 | Deleted and archived | `DELETE /api/session/<id>` is 204; then `GET`, `POST .../prompt` and `GET .../message` all answer 404 with body `{"_tag":"SessionNotFoundError","sessionID":"<id>","message":"Session not found: <id>"}` (an id that never existed answers identically). `PATCH /api/session/<id> {"archived":true}` is 204 and `GET` then shows `time.archived` as a millisecond number (null or absent otherwise); `GET /api/session` still lists archived sessions. A `steer` prompt to an archived session is accepted (200) and runs a turn; the session stays archived-listed until `PATCH {"archived":false}` (204) |
| 5 | Two TUIs on one session | Both attached with `opencode -s <id>`. An API prompt appeared once in each screen and started one turn; text typed and submitted in TUI 1 appeared in TUI 2 and ran once; an unsubmitted draft in TUI 2 was left untouched by an API prompt and by TUI 1's prompt. `GET .../message` showed each prompt exactly once. No conflict seen |
| 6 | `--standalone`, `--server` | `opencode --standalone` runs a private child (`opencode serve --stdio --port 0`) listening on a random loopback port (59997) that answers 401 without a credential; `service.json` was byte-identical before and after, still naming the service. `opencode serve --port 49400` (no `--service`) also left `service.json` unchanged. `opencode --server <url>` needs `OPENCODE_PASSWORD` for such a server and exited with that error. Such instances are invisible to `service.json`: the documented limit holds |
| 7 | Plan section 7 row 7 | `next_turn` with plain `queue` wakes an idle session (item 2). `next_turn` with `queue` + `resume:false` stores the message without a turn (item 3), so a Q6/D5 no-wake route exists; the stored message is delivered only when something later prompts that session, so an idle OpenCode session that is never prompted again never sees it |

### Evidence excerpts (key and password never appeared; session ids are scratch)

Item 3, idle session, `{"text":"M6-QR queue+resume:false marker","delivery":"queue","resume":false}`:

```
HTTP 200 {"data":{"id":"msg_…","sessionID":"ses_…","type":"user","payload":{"text":"M6-QR queue+resume:false marker"},"delivery":"queue"}}
GET /api/session/active   -> {"data":{}}      (6 s later)
GET /api/session/<id>/message -> []
GET /api/session/<id>/inbox   -> [{"delivery":"queue","text":"M6-QR queue+resume:false marker"}]
```

Item 4, deleted session:

```
DELETE /api/session/<id>  -> 204
GET    /api/session/<id>  -> 404 {"_tag":"SessionNotFoundError","sessionID":"<id>","message":"Session not found: <id>"}
```

Item 4, archived session: `GET /api/session/<id>` returns `"time":{"created":…,"updated":…,"archived":1795466749195}`; `POST .../prompt` returns 200 with a message id and `GET .../message` then ends `["idle","assistant","user"]`.

Item 5, TUI 2 after an API prompt and a prompt typed in TUI 1 (draft untouched):

```
┃  M6-T2 API prompt: reply with the single word PONG        PONG
┃  M6-T3 typed in t1 reply TYPED                            TYPED
┃  draft in t2 not submitted
```

## M4: install mechanics

Run 2026-11-23 with OpenCode 2.4.1 under a scratch `HOME` and `XDG_*` directories (`OPENCODE_DISABLE_AUTOUPDATE=1`, `OPENCODE_DISABLE_MODELS_FETCH=1`, `OPENCODE_DISABLE_CLAUDE_CODE=1`); no model, no operator config.

- `opencode mcp add crosstalk --global -- /abs/crosstalk mcp` on an `opencode.json` with a `//` comment and a `/* */` comment and a trailing comma keeps every comment and the trailing comma, and appends `mcp.servers.crosstalk = {"type":"local","command":["/abs/crosstalk","mcp"]}` (reformatted with two-space indent for the new block only).
- `--env KEY=VALUE` exists on `mcp add` and writes `"environment": {"KEY":"VALUE"}` in the same entry, so `CROSSTALK_NO_AUTO_REFRESH=1` needs no second edit.
- A second add of the same name replaces the entry (rc 0), so the CLI is idempotent but not a no-op; CrossTalk therefore checks first and skips the add when the entry is already right.
- With no config file it creates `opencode.json` containing only the `mcp` block. With only `opencode.jsonc` it edits that file; with both `opencode.json` and `opencode.jsonc` it edits `opencode.json` and leaves the other.
- A v1-shaped entry `mcp.crosstalk` is not touched: the add puts `mcp.servers.crosstalk` beside it, which is the K2 both-shapes failure, so install checks for it before the add.
- `"mcp": {"servers": "nope"}` makes the CLI exit 1 ("Can not add index to parent of type string") and leave the file as it was.
- `opencode mcp list` printed "No MCP servers configured" after an add, so it is not what `Registered` or K2 read; they read the config files.

Choice (D8): add through the CLI with `--env`, after CrossTalk has parsed the file, refused a conflicting entry and checked that `mcp.servers` is an object; then re-read the file and, if the entry is not exactly the installed command and environment, restore the bytes taken before the run. Uninstall edits the JSON text itself, only when the file has no comments.

M3 (does v2 prompt for CrossTalk's MCP tools) needs a model and was not run. The published config schema (`https://opencode.ai/config.json`) names permission keys for built-in tools and takes any other key as a rule, and states no default for MCP tools, so the default cannot be read offline. Install adds no allow rule and no K4; M3 stays open for slice 9.

## Defects for the coordinator

Listed, not fixed here.

1. **`next_turn` is sent as plain `queue`, contradicting ADR 0033 decision 2 and plan D5.** `internal/harness/opencode/inject.go` builds the body from `{text, delivery}` only and never sends `resume`. ADR 0033 says `next_turn` maps to `queue` with `resume: false` if measurement shows it stores without a turn; item 3 shows it does. As built, a `next_turn` inform to an idle session starts a paid model turn, the cost ADR 0033's consequences and plan Q6 accept only when no no-wake route exists. The code's state mapping also reports `queued` for it, which on an idle session is false: it starts running. If the fix is adopted, the stored message waits for the next prompt, which changes the "an inform wakes an idle OpenCode session" caveat planned for slice 8's docs and Q6; that is a coordinator decision.
2. **A prompt to an archived session is accepted and runs a turn.** ADR 0034 consequences say a send to a deleted or archived session is `unreachable` before the sweep drops it. The service does not refuse an archived target (200), and `Inject` checks no archived field, so a send to an archived session before the next sweep is delivered (`accepted`) and spends a turn. Only the deleted case is `unreachable` (404).
3. Consistent with the code, no action: the 404 body and status, `time.archived` as a number, `GET /api/session` including archived sessions (the D4 filter is needed), `service.json` pid equal to the listener pid, and the response `delivery` values `steer` and `queue`.
