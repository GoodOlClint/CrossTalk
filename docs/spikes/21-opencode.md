# Spike 21: OpenCode as a CrossTalk harness

Date: 2026-11-23. Issue #273 step 1 (parent #272). No product code.

Versions tested, both on macOS arm64, installed with `npm install --prefix <scratch>` (never globally):

| Line | npm package | Version | Source tag and commit |
|---|---|---|---|
| v1 (npm `latest`) | `opencode-ai` | 1.19.6 | `v1.19.6`, `f4e363c8295f` (anomalyco/opencode) |
| v2 | `@opencode/cli` | 2.4.0 | `v2.4.0`, `44aed4fd1446` (same repository) |
| Kilo CLI (source read only) | `@kilocode/cli` | 7.8.32 | Kilo-Org/kilocode `bfdec5496922` |

Terms. **TUI** = OpenCode's terminal interface. **Server** = OpenCode's HTTP API, which the TUI and other clients use. **v2 service** = the shared background server process (`opencode serve --service`) that every v2 TUI connects to. **MCP** = Model Context Protocol; **`_meta`** = the metadata object on an MCP request. **Probe** = a throwaway Python stdio MCP server that logged every frame, its parent pid, the parent's listening TCP sockets (`lsof`) and its environment (values other than `OPENCODE_*` and `XDG_*` replaced by their length). **Fake model** = a throwaway OpenAI-compatible HTTP server on 127.0.0.1, registered as a custom provider (`@ai-sdk/openai-compatible`), that answered with scripted text or tool calls and logged every request, so the exact role and position of each message the model receives is recorded. **Steer** = a message that joins the running turn at the next model call; **queue** = a message that waits until the turn ends.

Isolation. `HOME` and every `XDG_*` directory pointed into the session scratchpad, plus `OPENCODE_DISABLE_AUTOUPDATE=1`, `OPENCODE_DISABLE_MODELS_FETCH=1` and `OPENCODE_DISABLE_CLAUDE_CODE=1`; no real user config was read or written. Each TUI ran in a private tmux server. No model provider credential was used and none was present: the fake model gave live turns with no login and no outside party. Raw logs stayed in the scratchpad and are not committed; the excerpts below are the record, with session ids shortened and no password value.

## Verdict

- **Live delivery: yes**, on both lines, through the server API. A prompt sent to a session the TUI is showing appears in that TUI and starts a turn when idle; sent mid-turn it is shown at once and reaches the model at the next model call (between tool steps), or after the turn when the turn has no further step.
- **Identity per request: v2 yes, v1 no.** v2 sends `_meta["ai.opencode/sessionID"]` on every `tools/call`; v1.19.6 sends only `progressToken`. Kilo sends none.
- **Port discovery:** v2: read `$XDG_STATE_HOME/opencode/service.json` (url, pid, password; mode 0600), and the MCP's parent pid is the service pid in that file. v1: a plain `opencode` TUI opens **no socket at all** (in-process worker transport), so it is reachable only when started with `--port`/`--hostname`, or as `serve` plus `attach`; then the parent pid's listening socket gives the port.
- **Trust concern:** the injected text is a plain **user** message to the model; no API delivers it as a tool result or a weaker role. v1's prompt body also accepts `system`, which is appended to the system prompt, and its server has no credential unless the user sets `OPENCODE_SERVER_PASSWORD`, so with `--port` any local process of any user can inject user and system text.
- **Biggest open question:** v1. Most users are on v1 (the npm `latest` tag); there, without `--port`, nothing outside the process can reach the session, and even with it the MCP request carries no session id. Whether to support v1 at all, and how (require `--port` plus a password, or a plugin), is the change plan's first decision.

## Q1. Does an API prompt to the session the TUI shows appear there and start a turn?

**v1 1.19.6** (TUI started with `--port 18601`, session created by typing in the TUI):

| Request | HTTP | TUI | Model input (last messages) |
|---|---|---|---|
| idle: `POST /session/<id>/prompt_async {"parts":[{"type":"text","text":"INJECTED-IDLE from peer"}]}` | 204 | shown as a prompt, then `ACK: INJECTED-IDLE from peer` | `user \| INJECTED-IDLE from peer` |
| mid-turn, during a 20 s streamed text answer | 204 | shown at once with `QUEUED`; answered after the running answer ends | next call: `…assistant \| SLOW start 0 1 2…`, `user \| INJECTED-MIDTURN from peer` |
| mid-turn, while the model was choosing a tool call | 204 | shown; the turn continued with it | next call: `tool \| probe_echo: {"text": "hi"}`, `user \| INJECTED-BETWEEN-STEPS` |
| `POST /tui/append-prompt {"text":"TUI-APPENDED from peer"}` then `POST /tui/submit-prompt` | 200 `true`, 200 `true` | text typed into the prompt box, then submitted; same as a typed prompt; mid-turn it shows `QUEUED` | `user` |
| `/tui/append-prompt` while the operator had typed `operator half typed` (not submitted) | 200 | box became `operator half typed PEER-TEXT` | n/a |

`GET /session/status` during the slow turn: `{"ses_…":{"type":"busy"}}`; idle: `{}`.

The TUI routes write into the operator's own prompt box and merge with a half-typed draft, the same hazard as typing into a terminal; `prompt_async` does not touch the box. Source: `/tui/append-prompt` publishes `TuiEvent.PromptAppend` on the bus, and every TUI attached to that server applies it if its workspace matches (`packages/opencode/src/server/routes/instance/httpapi/handlers/tui.ts:34-38`, `packages/tui/src/component/prompt/index.tsx:237-239`). A busy `prompt_async` persists the user message and attaches to the running loop, which re-reads messages each step (`packages/opencode/src/session/prompt.ts:1056,1110-1115`, `src/effect/runner.ts:120-122`).

**v2 2.4.0** (plain `opencode`, which connects to the v2 service). There are no `/tui/*` or `prompt_async` routes; the API is under `/api`. `POST /api/session/<id>/prompt {"text":…,"delivery":"steer"|"queue"}` returns the admitted message, default `"delivery":"steer"`:

```
{"data":{"id":"msg_…","sessionID":"ses_…","time":{"created":…},"type":"user","payload":{"text":"V2-IDLE from peer"},"delivery":"steer"}}
```

| Request | TUI | Model input |
|---|---|---|
| idle, `prompt` | shown, answered | `user \| V2-IDLE from peer` |
| mid-turn (20 s streamed answer), `delivery: steer` | shown at once; `GET /api/session/<id>/inbox` lists it until delivered | next call, after the running answer: `user \| V2-STEER from peer` |
| mid-turn (tool step), `steer` and `queue` sent together | both shown | call 2: `tool \| probe_echo…`, `user \| V2-STEER-STEP`; call 3 (after that answer): `user \| V2-QUEUED` |

`GET /api/session/active` while busy: `{"data":{"ses_…":{"type":"running"}}}`; idle: `{"data":{}}`. Source: steer is promoted at the next step boundary, queue waits until idle (`packages/core/src/session/inbox.ts:52-57`; default `steer` at `packages/core/src/session/prompt.ts:52`).

## Q2. How can a child MCP server find the server's port?

**v1.** A plain `opencode` TUI runs the server in a Bun worker and talks to it over in-process RPC (`url: "http://opencode.internal"`, `fetch: createWorkerFetch(client)`); it listens only when `--port`, `--hostname` or `--mdns` is given, and the config file cannot turn it on (`packages/opencode/src/cli/cmd/tui.ts:272-288`, `src/cli/tui/worker.ts:42,54-57`). Measured, plain TUI: the probe's parent was the `opencode` process and `lsof -iTCP -sTCP:LISTEN` on it was empty; the only socket was an outbound connection. With `--port 18601`, the same lookup from the probe found it:

```
"parent_listen": "opencode. <ppid> … TCP 127.0.0.1:18601 (LISTEN)"
```

No variable carries the URL. The MCP environment had `OPENCODE=1` and `OPENCODE_PID=<parent pid>` (set at `packages/opencode/src/index.ts:75-77`), nothing with a port. `--port 0` (the default value) means 4096 first, then random (`src/server/server.ts:117-122`). `opencode serve` plus `opencode attach <url>` is the documented split form.

**v2.** Every client uses real HTTP to the v2 service, which plain `opencode` starts if none is running (`packages/cli/src/commands/handlers/default.ts:49`, `src/services/service-config.ts:150`). It binds `127.0.0.1` on a fixed port, 0xC0DE = 49374, for the `latest`, `dev`, `beta` and `next` channels (`service-config.ts:57`), and writes a registration file. Measured:

```
$XDG_STATE_HOME/opencode/service.json  -rw-------  {id, version: "2.4.0", url: "http://127.0.0.1:49374", pid: <service pid>, password: <43 chars>}
$XDG_CONFIG_HOME/opencode/service.json -rw-------  {password: <43 chars>}
```

The probe's parent was the service (`opencode serve --service`), listening on 127.0.0.1:49374, and its pid matched `service.json`. The service outlived the TUI that started it (reparented to pid 1); a second and third TUI reused it. `opencode --standalone` instead runs a private `serve --stdio --port 0` child with a random password (`src/services/standalone.ts:17-24`), and `--server <url>` connects elsewhere; those were not measured.

## Q3. Is the session id in the MCP request `_meta`, and what else identifies the session?

| Line | `tools/call` frame (probe log) |
|---|---|
| v1 1.19.6 | `{"name":"probe_echo","arguments":{"text":"hi"},"_meta":{"progressToken":2}}` |
| v2 2.4.0 | `{"name":"probe_echo","arguments":{"text":"hi"},"_meta":{"ai.opencode/sessionID":"ses_…","progressToken":2}}` |

Source: v1 calls `client.callTool({name, arguments}, …)` with no `_meta` (`packages/opencode/src/mcp/catalog.ts:54-58`); v2 adds `_meta: {"ai.opencode/sessionID": input.sessionID}` when a session is known (`packages/core/src/mcp/client.ts:884`). The v2 id is the same `ses_…` the API uses.

Other identity facts:
- `initialize` `clientInfo`: v1 `{"name":"opencode","version":"1.19.6"}`, protocol `2025-11-25`. v2 also sends a `server/discover` request.
- v1: one MCP process per TUI process, which can show several sessions in turn, so the process alone does not name the session. The plugin hooks `chat.message` and `tool.execute.before` see `sessionID` (`packages/plugin/src/index.ts:222-296`), the only in-process path to it on v1.
- v2: one MCP process per project directory inside the shared service, serving every session in that directory: four probe processes ran under one service after TUIs and CLI commands ran in four directories. The MCP inherits the service's environment, not the TUI's (`packages/core/src/mcp/stdio.ts:103-107`); shell-tool children get `OPENCODE_SESSION_ID`, MCP children do not (`packages/core/src/tool/plugin/shell.ts:480-483`).
- v2 routes MCP tools through one model-facing `execute` tool ("Code Mode"): the model writes JavaScript such as `return await tools.probe.probe_echo({text: "hi"})`, and the catalog in the system prompt lists `tools.probe.probe_echo({text?: string})`. CrossTalk's tool descriptions therefore reach the model as catalog lines, not as first-class tools.

## Q4. Credential, bind address, and who can reach the server

| | v1 1.19.6 | v2 2.4.0 |
|---|---|---|
| Bind | `127.0.0.1` (TUI with `--port`, and `serve`) | `127.0.0.1:49374` |
| Credential | none unless `OPENCODE_SERVER_PASSWORD` is set; then HTTP Basic, user `opencode` (or `OPENCODE_SERVER_USERNAME`) or `?auth_token=` | always: HTTP Basic, user `opencode`, password from `service.json` |
| Measured | TUI `--port`: `GET /session` 200 with no credential. `serve` with a password: no credential 401 (`www-authenticate: Basic realm="Secure Area"`), right password 200, wrong 401 | `/api/*` and `/openapi.json`: 401 without, 200 with the password. Paths outside `/api` (`/session`, `/doc`, `/event`) return 200 with the web app's HTML, not API data |

Source: v1 `packages/opencode/src/server/auth.ts:17-20`, `middleware/authorization.ts:46,78-80`, `cli/cmd/serve.ts:15-16` (prints a warning when the password is unset); v2 `packages/cli/src/server-process.ts:91-97`, `packages/server/src/auth.ts:20`. CORS on both allows `localhost:*`, `127.0.0.1:*`, `*.opencode.ai` and `--cors` origins (`packages/server/src/cors.ts:11-19`).

Who can reach it: a loopback TCP port has no owner check, so on v1 without a password every process of every local user can call the API, including `prompt_async` and its `system` field. On v2 the password file is mode 0600, so only the same user (and root) can read it; any process of the same user can. That matches CrossTalk's accepted same-user model (ADR 0004 Consequences). Reach from a second local account was not measured (no second account in this run): `sudo -u <other-user> curl -s -o /dev/null -w '%{http_code}' 127.0.0.1:<port>/session` against a v1 `--port` TUI is the command.

## Q5. What position does an injected message take for the model?

Both lines deliver the text as a **user** message, the same role as the operator's typed prompt. Variants measured on v1 (one request each, positions from the fake model's log, `MARK` locates the text):

| v1 body | Model sees | TUI shows |
|---|---|---|
| plain text part | `user` message | yes |
| text part with `"synthetic": true` | same `user` message (`[25, "user", 17]`) | **no**: only the non-synthetic part was rendered |
| `"noReply": true` | stored; no model call (0 calls); joins the next turn as `user` | yes |
| `"system": "MARK-SYSTEM-FIELD"` | appended to the **system** message (`[0, "system", 9645]`, its end) | no |

v2: `POST /api/session/<id>/synthetic {"text":"MARK-V2-SYNTH from peer"}` (200) started a turn; the model got it as `user` (`[17, "user", 0]`) and the TUI showed only the answer, not the text. `resume: false` is v2's `noReply`.

Source: all v1 parts become a user message; `ignored` drops a part, `synthetic` affects only title and display (`packages/opencode/src/session/prompt.ts:203`, `src/session/message-v2.ts:221`); `system` is taken from the latest user message (`src/llm/request.ts:61`). v2 renders `synthetic` as user (`packages/core/src/session/runner/to-llm-message.ts:274-275`). There is no endpoint that adds an assistant, system or tool-result message, except v2's experimental `PUT /api/experimental/session/<id>/instructions/entries/<key>`, which by source appends a system message (`packages/core/src/session/message-updater.ts:154-163`; not measured).

Meaning for ADR 0004. "Peer text never enters developer or system instructions" holds only if the harness package never sets v1's `system` field, never uses the v2 instructions endpoint and never uses a plugin's system transform; the package must send a plain user prompt. Visibility: `synthetic` hides the peer text from the operator while the model still reads it as user input, so it must not be used either; the operator should see what the model sees. A user-role message is stronger than Codex's tool-output delivery (ADR 0007) and the same as Claude's channel delivery, so the envelope and notice (`msg.PeerNotice` and the information notice) are the only marker that the text is a peer's, not the operator's. On v1 without a password, any local process can do the same injection and set `system` without CrossTalk, so the package adds no new hole there, but it cannot attribute a message it did not send.

## Q6. Listing sessions, status, end and replacement

- **v1** `GET /session`: `id`, `slug`, `projectID`, `directory`, `title`, `agent`, `model`, `version`, `time{created,updated}` (`packages/schema/src/v1/session.ts:543-568`); it is session history from SQLite (`$XDG_DATA_HOME/opencode/opencode.db`), not running sessions. Busy: `GET /session/status` (busy, retry; idle sessions absent). `GET /global/health`: `{"healthy":true,"version":"1.19.6"}`. `GET /path` returns the home, state, config and directory paths.
- **v2** `GET /api/session`: `id`, `parentID`, `projectID`, `agent`, `model`, `title`, `location{directory}`, `time{created,updated,busy,idle,viewed,archived}` (`packages/schema/src/session.ts:32-62`). Running: `GET /api/session/active` ("foreground Session drains currently owned by this OpenCode process"). Undelivered input: `GET /api/session/<id>/inbox`. Events: `GET /api/event` (SSE, `server.connected` then heartbeats).
- Which session a TUI is **showing** is not exposed on either line: v1 has `POST /tui/select-session` but no getter, v2 has `POST /api/session/<id>/view` (marks an idle transition as viewed) and no getter. A session can be in the list with no TUI on it, and on v2 the service keeps serving after every TUI exits.
- End and replace: `DELETE /session/<id>` (v1), `DELETE /api/session/<id>` (v2), `opencode session delete`; archive by setting `time.archived`. A v1 TUI's server ends with its process; a TUI started with `--session <id>` or `--continue` reopens an existing session, and `--fork` makes a new one. v2 sessions outlive TUIs.

## Q7. Registering and removing an MCP server

- **v1** config, merged: `~/.config/opencode/{config.json,opencode.json,opencode.jsonc}`, `OPENCODE_CONFIG`, project `opencode.json[c]` up to the worktree root, `.opencode/` and `OPENCODE_CONFIG_DIR` (`packages/opencode/src/config/config.ts:272-274,413-445`). Shape: `"mcp": {"<name>": {"type": "local", "command": ["<abs>/crosstalk", "mcp"], "environment": {…}, "enabled": true}}`. CLI: `opencode mcp add|list|auth|logout|debug`; **no remove**. `opencode mcp add ctx --global -- /usr/bin/true mcp` printed its option list and wrote nothing (v1 `add [name]` takes no command argument).
- **v2** shape: `"mcp": {"servers": {"<name>": {"type": "local", "command": [...], "environment": {…}, "disabled": false}}}` (`packages/schema/src/config/mcp.ts:19-22`). Measured: `opencode mcp add ctx --global -- /usr/bin/true mcp` wrote `mcp.servers.ctx` into `$XDG_CONFIG_HOME/opencode/opencode.json`. CLI `add [--global]|list|auth|logout`; **no remove**. v2 also spawned the probe from the v1 shape (`mcp.<name>`), so it still reads it. `opencode mcp list` printed `No MCP servers configured` in both cases while the probe was connected; not investigated. v2 also reads `.claude`, `.agents` and `.opencode` in each ancestor directory and an MDM plist (`packages/core/src/config/discovery.ts:70-93`).
- So `crosstalk install opencode` would edit the global JSON (JSONC) file itself, for both add and remove, as it does for Codex's TOML.

## Q8. Kilo CLI (source read only)

From Kilo-Org/kilocode `bfdec5496922` (`packages/opencode/package.json` 7.8.32), paths under `packages/opencode/`:
- Routes kept: `/session`, `/session/status`, `/session/:id/message`, `/session/:id/prompt_async`, `/tui/append-prompt`, `/tui/submit-prompt`, `/event` (`src/server/routes/instance/httpapi/groups/session.ts`, `groups/tui.ts`, `groups/event.ts`). Kilo adds `/kilo/...` routes (`src/kilocode/session-hosting/native-routes.ts`).
- Plain `kilo` is like v1: in-process worker transport (`http://kilo.internal`), no listener unless `--port`/`--hostname` (`src/cli/cmd/tui.ts:435-447`). Kilo adds a daemon the TUI attaches to when one runs, disabled by `KILO_NO_DAEMON` (`src/kilocode/cli/cmd/tui/thread.ts`, `src/kilocode/daemon/`); its port and autostart are unverified.
- Auth renamed: `KILO_SERVER_PASSWORD`, `KILO_SERVER_USERNAME` (default `kilo`) (`packages/core/src/flag/flag.ts:68`); the TUI's worker uses a random password (`thread.ts:57-64`).
- MCP: same `mcp.<name>` local shape; config names `config.json`, `kilo.json[c]` and the old `opencode.json[c]` (`packages/core/src/config.ts:144`), under `~/.config/kilo`. **No `_meta` session id** on `tools/call` (`src/mcp/catalog.ts:55-66`).
- Binaries `kilo` and `kilocode`, environment prefix `KILO_`.

So the v1 findings apply to Kilo with renamed paths and variables; the v2 identity path does not. Runtime is unverified: `npm install --prefix <scratch> @kilocode/cli@7.8.32`, then with scratch `HOME`/`XDG_*` run `kilo --port 18700` in a terminal and repeat the Q1 to Q3 requests (`curl 127.0.0.1:18700/session`, `prompt_async`, and the probe's `tools/call` frame).

## Not run

- A real model. The fake model shows exactly what the model is given; how a real model treats a peer's user message (and the notice) was not observed.
- Reach from a second local user (command in Q4).
- v2 `--standalone` and `--server`, and the v2 experimental instructions endpoint.
- How many sessions one v2 MCP process serves under load, and what happens to it when its directory's last session ends.

## What a harness package needs

Mapping onto `internal/harness/harness.go`. Open design questions are marked **Q:**; no decision is made here.

- `Name()`: `opencode`. **Q:** one package for both lines and Kilo, or v2 only first.
- `Identify` / `Bind` (identity per request): v2 reads `params._meta["ai.opencode/sessionID"]` on each `tools/call`, as Codex reads `threadId`; `unbound` when absent. One MCP process serves several sessions in a directory (the Codex pooling case). v1 and Kilo give no session id. **Q:** on v1, accept `unbound` until the model calls `claim`-style self-identification, or ship a small OpenCode plugin that records `sessionID` from `tool.execute.before` for the shim, or treat v1 as unsupported.
- `Discover`: v2: read `$XDG_STATE_HOME/opencode/service.json` (respect `XDG_STATE_HOME` and channel-specific files), call `GET /api/session` and `GET /api/session/active` with the password; name from `title`, cwd from `location.directory`, busy from `active` or `time.busy`/`time.idle`. **Q:** which sessions count as agents: every stored session is in the list, and none is marked as shown in a TUI, so a filter (active, recently updated, or seen in a `tools/call`) is needed. v1: only TUIs started with `--port`; the shim learns the port from its parent's listening socket, and only for its own session.
- `Render` and inject: v2 `POST /api/session/<id>/prompt` with `{"text": <envelope>, "delivery": "steer"}`; v1 `POST /session/<id>/prompt_async` with one plain text part. Never `system`, `synthetic`, the instructions endpoint or the `/tui/*` routes (they edit the operator's draft). Delivery goes through the receiver chain like every other harness. **Q:** steer or queue for `inform` versus `ask`; whether `delivery` maps onto CrossTalk's `immediate`/`next_turn`.
- `NativeReplyAddress`: none found; OpenCode has no peer channel of its own.
- `ClaimSource` / `SessionRegistrar`: v2 can register sessions from `service.json` plus the API at shim start, before any call, like the Claude session-file poll. **Q:** poll `GET /api/session/active` or subscribe to `GET /api/event`.
- `Supersession`: a session deleted with `DELETE /api/session/<id>` or archived (`time.archived`) is gone; a v1 TUI's server dies with its process. **Q:** how a CrossTalk descriptor is retired when a v2 session is only idle for days, since the service never exits.
- `Installer`: edit the global `opencode.json[c]` (v1 `mcp.<name>`, v2 `mcp.servers.<name>`; v2 reads both) with `type: local`, `command: ["<abs>/crosstalk", "mcp"]`; remove by editing the file, since neither CLI removes. Checks: binary on PATH and version, entry present, v2 `service.json` readable. **Q:** JSONC comments must survive the edit; which file wins when a project `opencode.json` also names `crosstalk`; whether OpenCode's permission config needs an allow rule for the CrossTalk tools (not measured).
- Tool surface: on v2 the model calls CrossTalk tools through Code Mode `execute`, so tool descriptions and the skill text should be checked for how they read as catalog lines. **Q:** whether the CrossTalk skill or plugin has an OpenCode form.
- `Refreshable`, `BackendChecker`, `NativeIngress`: no OpenCode-specific need found.
