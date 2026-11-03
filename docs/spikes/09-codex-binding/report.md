# Codex self-binding: live probe results

**The proposed targeted call works on Codex 0.166.0. A second app-server client can call a thread's MCP tool while that thread is idle or active, without `experimentalApi`. The receiving tool request already contains `_meta.threadId`; a normal model-issued call additionally contains `_meta.sessionId`. Prefer that metadata to nonce-based inference. Elicitation did not reach an unrelated observer connection.**

MCP means Model Context Protocol. A thread is a Codex conversation. A nonce is a unique test string used to match a request to its receipt. Process identifiers distinguish operating-system processes. The control endpoint was `/Users/goodolclint/.codex/app-server-control/app-server-control.sock`, served by process 83168. Four independent WebSocket clients were used: `creator`, `caller`, `observer`, and `nonexperimental`. Only the last initialized with `experimentalApi:false`.

## Results

| Question | Observed result |
|---|---|
| Does a second client reach the thread's instance? | Yes. Two fresh idle threads reached separate probe processes 93091 and 93120. Their returned nonce and `_meta.threadId` matched the targeted thread. Subsequent calls reused those processes. |
| Does it work before the thread has used the server? | Yes. Each fresh thread had no model turn or prior tool call. The first external call was sent before its probe process started; it waited for initialization and completed. Thread creation also initiates startup, so this does not isolate the call as the sole cause of process creation. |
| Does it work during an active turn? | Yes. `thread/read` showed this thread active immediately before the second-client call; that call reached process 93073. A subsequent normal model tool call reached the same process. |
| Is `experimentalApi` required? | No for `mcpServer/tool/call`: the independently initialized false-capability client succeeded against both fresh threads. |
| Can an unrelated client discover binding through elicitation? | Not in this test. `elicitation/create` became an app-server **request**, with an identifier requiring a response, addressed to the subscribed creator. Neither the calling client nor the unrelated observer received it during the three-second observation window. The creator declined it and the tool completed. |

The fixture explicitly negotiated MCP protocol `2025-06-18`. Separate processes are proven for this tested configuration, not for all protocol versions; the release-source connection-pooling qualification in the earlier research still applies.

## Exact methods and representative raw frames

Each app-server client sent `initialize`, waited for success, and sent `initialized`. The nonexperimental client's request was:

```json
{"id":"nonexperimental-1","method":"initialize","params":{"clientInfo":{"name":"crosstalk_binding_probe_nonexperimental","version":"0.1"},"capabilities":{"experimentalApi":false}}}
```

The temporary global MCP entry was loaded with `config/mcpServer/reload {}`; the response was `{"id":"caller-2","result":{}}`. No daemon restart was performed. `thread/start` created unused idle threads with `cwd` set to the probe artifact directory, `ephemeral:true`, and other configured MCP servers disabled only in those temporary threads.

Second-client request for the first idle thread:

```json
{"id":"caller-5","method":"mcpServer/tool/call","params":{"threadId":"01a1a33c-9dbe-74d0-9309-f9e6d8e10407","server":"crosstalk-probe","tool":"_bind","arguments":{"nonce":"idle-0-1793740021226817000"}}}
```

The receiving standard-input/output MCP frame, logged by process 93091:

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"_meta":{"threadId":"01a1a33c-9dbe-74d0-9309-f9e6d8e10407","progressToken":1},"name":"_bind","arguments":{"nonce":"idle-0-1793740021226817000"}}}
```

The app-server response contains `content`, `structuredContent`, and `isError:false`. The exact complete response, without manually reconstructing its duplicated text field, is preserved in [selected-frames.json](crosstalk-binding-probe/selected-frames.json). Its `structuredContent` is:

```json
{"nonce":"idle-0-1793740021226817000","pid":93091,"ppid":83168,"meta":{"threadId":"01a1a33c-9dbe-74d0-9309-f9e6d8e10407","progressToken":1}}
```

`mcpServerStatus/list {"threadId":"01a1a33c-9dbe-74d0-9309-f9e6d8e10407","serverName":"crosstalk-probe"}` returned one `connected` entry exposing `_bind` and `_elicit`. The false-capability client then called `_bind` on that thread and received the same process 93091 with the correct thread identifier. All request identifiers, nonces, parameters and raw responses are in the selected-frames artifact.

On server initialization, the captured MCP `clientInfo` was `{"name":"codex-mcp-client","title":"Codex","version":"0.166.0"}`. The request advertised elicitation form and URL support but contained no thread identifier. The fixture's selected environment-variable log also contained no `CODEX_HOME`, `CODEX_THREAD_ID` or `CODEX_SESSION_ID`.

## Normal model call: direct identity verified

After registering the probe, I invoked its `_bind` tool normally from this conversation with nonce `normal-model-tool-call-2026-11-03`. It reached process 93073, the same instance reached by the external active-thread call. The receiving `_meta` included:

```json
{"threadId":"01a1a300-21dc-7fe2-870a-d43593ff0992","sessionId":"01a1a300-21dc-7fe2-870a-d43593ff0992"}
```

It also carried `callId`, `x-codex-turn-metadata`, `windowId`, `itemId`, and `progressToken`. The full result is [model-tool-result.json](crosstalk-binding-probe/model-tool-result.json), and the unmodified incoming frame is in [server-93073.jsonl](crosstalk-binding-probe/server-93073.jsonl). This closes the previous report's missing wire-capture check.

## Elicitation routing and limits

The fixture sent `elicitation/create` in response to `_elicit`. The creator received:

```json
{"method":"mcpServer/elicitation/request","id":0,"params":{"threadId":"01a1a33c-9dbe-74d0-9309-f9e6d8e10407","turnId":null,"serverName":"crosstalk-probe","mode":"form","_meta":null,"message":"Disposable CrossTalk routing probe; decline is expected.","requestedSchema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}}}
```

The creator answered `{"id":0,"result":{"action":"decline","content":null,"_meta":null}}`. The MCP server received `{"jsonrpc":"2.0","id":"probe-elicitation-93091-1","result":{"action":"decline"}}`; the caller's `_elicit` request then completed. The calling client did not become an elicitation subscriber merely by calling the tool.

An attempted follow-up, joining an unused test thread with `thread/resume {threadId, excludeTurns:true}`, failed with `-32600: no rollout found for thread id …`. This happened for both an ephemeral thread and a non-ephemeral thread with no model turn. I stopped that branch without starting model work. Therefore this probe does not establish elicitation delivery to an independently subscribed second client, nor startup-time unsolicited elicitation. The direct-call route already works without those dependencies.

## Design implication and cleanup

Use host-supplied `_meta.threadId` on each tool call. A nonce remains useful as a test correlation value, but the instance does not need a private nonce-to-thread map to discover the caller. If startup binding before any model tool call is required, the external targeted `_bind` route is viable; finish MCP initialization and tool discovery before attempting it, otherwise a startup callback can wait on its own unfinished handshake. Treat unsupported/direct-input-denied threads as failures to bind; do not select a candidate from activity or directory.

This tested an independent driver calling existing per-thread servers, not autonomous scanning from every server at startup. Multiple-daemon selection, forks, reconnection, automatic startup scan ordering and newer-protocol sharing remain separate tests. No implementation architecture was approved by this probe.

The global `crosstalk-probe` entry was removed and configuration reloaded. Final checks showed no probe processes, no probe entry in this thread's server inventory, and the original four loaded thread identifiers. One temporary thread lingered after a failed resume; archive/delete returned the missing-rollout error, but after the final deletion attempt the runtime entry was absent. The non-ephemeral test thread was archived. See [cleanup-final.json](crosstalk-binding-probe/cleanup-final.json) and [cleanup.json](crosstalk-binding-probe/cleanup.json).

Full evidence: [primary summary](crosstalk-binding-probe/summary.json), [complete app-server frames](crosstalk-binding-probe/app-server-frames.jsonl), per-process `server-*.jsonl`, [follow-up summaries](crosstalk-binding-probe/joined-summary.json), and the disposable fixture/driver scripts in the same directory. These are research artifacts, not bridge implementation code. No existing conversation was steered, and no daemon or harness was restarted.
