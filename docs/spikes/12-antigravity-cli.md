# Spike 12: Antigravity CLI as a CrossTalk harness

Date: 2026-11-04. agy (Google's Antigravity CLI) 1.5.0 on macOS, Homebrew cask, account on the "Antigravity Starter" quota (account identity removed). Model `gemini-3.8-flash-low`. Seven of eight budgeted prompts used. No global config touched: a throwaway workspace with `.agents/mcp_config.json` and `.agents/hooks.json`, plus `agy --settings <file> --log-file <file>`.

Terms. **MCP** = Model Context Protocol. **`_meta`** = the metadata object on an MCP request. **Hook** = a command agy runs at a lifecycle point, with JSON on stdin and stdout. **`injectSteps`** = hook output that adds steps to a conversation. **TUI** = agy's terminal interface. **Transcript** = agy's per-conversation JSON-lines log. **Invocation** = one call to the model. **Probe** = a throwaway Python MCP server that logged every frame and offered `probe_echo`.

Raw logs were in a temporary directory and are not committed. Excerpts below are the record. They contain no credentials; the hook and MCP logs recorded environment variable names and only the `ANTIGRAVITY_*` values, with long values replaced by their length.

## Measured

| Question | Result |
|---|---|
| MCP process parent | Probe pid 76020, parent pid 73463, parent command `agy` |
| Environment | No `ANTIGRAVITY_*` variable in the MCP process |
| `initialize` | `clientInfo {name:"antigravity-client", version:"v1.0.0"}` (not the CLI version), `protocolVersion 2025-11-25`, capabilities `elicitation{form,url}`, `roots{listChanged:true}` |
| `tools/call` `_meta` | `antigravity.google/artifacts_dir` (`~/.gemini/antigravity-cli/brain/<id>`), `antigravity.google/conversation_id`, `antigravity.google/root_conversation_id` (equal here), `progressToken "<uuid>:<n>"` |
| `/clear` (the menu lists it as `/clear`, `/new`) | New conversation `d80a4227-…`; same MCP pid 76020, no new `initialize`; `_meta` ids change. One process serves successive conversations |
| Allowlist | `--settings {"permissions":{"allow":["mcp(probe/*)"]}}` removed the MCP permission prompt |
| Model tool choice | "Call probe_echo" made the model run shell `echo` (with a shell permission prompt). "Call the MCP tool probe_echo (server probe), not the shell" worked: the model read `~/.gemini/antigravity-cli/mcp/probe/probe_echo.json`, then called `call_mcp_tool{ServerName,ToolName,Arguments}` |
| Hook load | Workspace `.agents/hooks.json` read: `agy.log` `loaded 0 named hooks from 0 hooks.json file(s)` at startup, then `loaded 1 named hooks from 1 hooks.json file(s)` once the workspace loaded |
| Hook environment | `ANTIGRAVITY_CONVERSATION_ID` (36 characters); each firing is a fresh process |
| Hook vs MCP identity | Hook `conversationId` = `_meta` conversation id = root id |
| `PreInvocation` stdin | `artifactDirectoryPath, conversationId, initialNumSteps, invocationNum, lastUserInput, modelName, transcriptPath, workspacePaths`. Fires before each model call, `invocationNum` 0 first |
| `Stop` stdin | `conversationId, error, executionNum, fullyIdle, lastUserInput, modelName, terminationReason, transcriptPath, workspacePaths` |
| `{}` hook output | Accepted on every firing with no effect |

## Injection forms

Each form was dropped in a file the hook consumed at `invocationNum` 0.

| Form | Transcript record | Visible in TUI | Model |
|---|---|---|---|
| `{"ephemeralMessage":"MAILBOX-E: the word is heron"}` | `source SYSTEM_SDK, type EPHEMERAL_MESSAGE`, placed after the `USER_INPUT` step | No | Quoted it exactly |
| `{"userMessage":"MAILBOX-U: the word is otter"}` | `source SYSTEM_SDK, type USER_INPUT`, wrapped in `<USER_REQUEST>` | Yes, as `> MAILBOX-U: …` | Quoted it; sees a user turn; differs from real input only by source (`USER_EXPLICIT`) |
| `{"toolCall":{"name":…,"args":…}}` | None. `agy.log`: `pre-invocation hook: failed to build injected steps from hook …: unknown injected step type: <nil>` | Error | **The turn failed**: `Stop` had `terminationReason ERROR`. The right schema is unknown |
| `systemMessage` | Not run | | |

The `toolCall` result matters beyond that form: a malformed hook output can end a turn.

## Stop re-entry

Hook output `{"decision":"continue","reason":"…"}` re-entered the loop. Transcript: `source SYSTEM, type SYSTEM_MESSAGE`, text "The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to. <SYSTEM_MESSAGE> Stop hook blocked termination: <reason> </SYSTEM_MESSAGE>". The model replied again. Timeline of the firing:

```
PreInvocation invocationNum 0
Stop          executionNum 0  NO_TOOL_CALL  fullyIdle true   -> continue
PreInvocation invocationNum 1
Stop          executionNum 1  NO_TOOL_CALL  fullyIdle true
```

All earlier and later turns, none of them re-entered, show `executionNum` 0. Whether it resets on the next prompt after a re-entry in the same conversation was not observed, since the next prompt was in a new conversation. The changelog also describes a built-in limit: "after a configurable number of consecutive continuations, the hook can no longer block".

## Docs against measurement

| Point | Docs | Measured |
|---|---|---|
| `terminationReason` | `"model_stop"`, `"max_steps_exceeded"`, `"error"` | `NO_TOOL_CALL`, `ERROR` |
| Transcript file | `…/logs/transcript.jsonl` | `transcriptPath` names `transcript_full.jsonl` |
| Global hooks in the CLI | The hooks page lists `~/.gemini/config/hooks.json` for the CLI, and the settings page agrees | **Not measured**; a secondary source said the CLI reads hooks only from plugins. Only the workspace file ran |
| Global MCP config | `~/.gemini/config/mcp_config.json` | Not exercised. The file exists on this machine at 0 bytes. `agy mcp list` offline said "No MCP servers configured" |
| `_meta` conversation keys | Changelog only: "Added `_meta["antigravity.google/root_conversation_id"]` to MCP `tools/call` requests so MCP servers receive the top-level conversation ID even when a tool is called from a nested subagent". Not on the MCP page | Present |
| Tool always loaded | Changelog only: `_meta: {"antigravity/alwaysLoad": true}` on a `tools/list` entry | Not tested |

## Read from docs, not tested

- Events: `SessionStart`, `PreToolUse`, `PostToolUse`, `PreInvocation`, `PostInvocation`, `Stop`. Handler `timeout` in whole seconds, default 30. MCP tool call limit 3 minutes, connect limit 5 minutes.
- `~/.gemini/config/` is shared by the App, CLI and IDE; `<app_data_dir>` differs (`antigravity-cli` for the CLI), which also shows in `artifacts_dir`.
- agy has a built-in `send_message` tool (`Recipient`, `Message`) for its own agents.
- `/schedule` runs in the conversation that created it; each firing is a model invocation.
- `agy agentapi` exists (`get-conversation-metadata`, `new-conversation`, `send-message`). The operator ruled it out; it was only listed, not run.
- Terms: "Using third party software, tools, or services to access the Service (e.g. using OpenClaw with Antigravity OAuth) is a breach of this Agreement." Google has stated no position on documented MCP or hook setups.

## Not run

Second `agy` process, subagent, mid-turn `PreInvocation` latency, idle drop before the very first prompt, `systemMessage`, `SessionStart`, process lifetime on exit (the session was left running), hook failure modes (missing binary, non-zero exit, timeout), and the parent pid of a hook. These are spikes S1–S6 in `docs/plans/antigravity-harness.md`.
