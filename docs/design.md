# CrossTalk design, draft 1.3 (2026-11-03)

Status: draft under operator review (greenfield gate step 4). Decisions are recorded in `docs/decisions/` (ADR 0001–0004). Evidence for every "verified" claim is in the 2026-11-03 design-session notes (spikes 1–4, cross-harness round trip).

Terms: harness = the agent CLI (Claude Code, Codex). Session = one running harness instance in a herdr pane. Agent = the registered identity of a session. Shim = the per-session process described in §2. A2A = Agent-to-Agent protocol. MCP = Model Context Protocol. uds = unix domain socket.

## 1. Goal and non-goals

Goal: any agent on this machine can send a message to any other by name and the receiver gets it inside its running session, without a human and without screen scraping, across harnesses.

Non-goals for v1: spawning agents (stays with herdr and a separate delegate tool), cross-machine messaging, kind=control (stop/shutdown), headless agents outside herdr, isolation against hostile same-user processes.

## 2. Components

| Component | Count | Owns |
|---|---|---|
| Shim | one per session | MCP server to its model; A2A server+client; cc-socks-compatible uds ingress; injector into its own harness; registration |
| Directory | none as a process | discovery = the shims' descriptor directory, enriched from each harness's own live registry (ADR 0003) |
| herdr | optional | panes and restore only; its pane id is recorded for the terminal fallback and nothing else |

The shim is a Go binary (ADR 0002), one package with a harness adapter inside it. Adding a harness = one adapter: `register()`, `inject(message, delivery)`, `status()`.

## 3. Registration and discovery

On start the shim writes `~/.local/state/crosstalk/agents/<name>.json`:

```
{ "name": "<role name>", "harness": "claude|codex", "role": "primary|subagent", "owner": "<name>|null",
  "pid": <shim pid>, "cwd": "...", "herdr_pane": "w1:p1", "session_ref": "<claude session id | codex thread id>",
  "a2a_url": "http://127.0.0.1:<port>", "uds_path": "/tmp/cc-socks/<shim pid>.sock",
  "instance_id": "<uuidv7, new per shim start>", "harness_endpoint": "<codex daemon socket path | claude socket path>",
  "credential_ref": "<path to 0600 file holding this agent's A2A bearer token>",
  "started_at": "...", "protocol": 1 }
```

Binding (F2): `harness_endpoint` + `session_ref` are the shim's explicit binding to exactly one conversation on exactly one server, fixed at start from the shim's own launch context (the pane it was started in, the thread id handed to it). A shim never selects its conversation by name, cwd or "most recent"; an ambiguous binding is a startup failure. Before any Codex delivery the shim confirms the thread is loaded on that endpoint and `canAcceptDirectInput` is true, and distinguishes idle / active / notLoaded / systemError.

Credentials (F5): the bearer token lives only in the file `credential_ref` points to (0600). Descriptors and `list_agents` output never contain it; logs redact it.

Publish order and ownership (F8): bind uds + A2A listener, confirm harness binding, then write the descriptor atomically (temp + rename). A live descriptor with the same name and a live pid of a different `instance_id` is a conflict: the newcomer refuses to start. On exit a shim removes its descriptor only if the file's `instance_id` is its own. Readers treat a descriptor as stale when the pid is dead or the pid is alive but its start time predates `started_at` mismatch (pid reuse).

Name is a stable role name given at launch; a restarted session re-registers under it and replaces the file. Role and owner are fixed at start and not model-changeable. `list_agents` reads this directory, prunes dead or reused pids, and enriches each row with live status from the harness's own registry (Claude: `~/.claude/sessions/<pid>.json` status; Codex: `thread/read` status and `canAcceptDirectInput`). herdr is not consulted for discovery. On exit the shim removes its own-instance file.

Claude-visible registration (R2, conditional support from Codex): the shim also publishes `~/.claude/sessions/<shim pid>.json` + `<shim pid>.<sha256>.key` as explicit compatibility registration, never impersonation: its own pid, its own socket, a freshly generated key (0600), a distinct name, `kind: "external"` only if Claude accepts that value, no borrowed Claude session id or token, no advertised capabilities it lacks. Publish only after the listener is ready; remove only its own record; handle pid reuse on stale cleanup. Gate: test 11 below. If Claude rejects or mis-handles the record, discovery stays in our `list_agents` and native replies are tested against the explicit `uds:` address only.

Harness-native registries the shim reads, never writes: `~/.claude/sessions/<pid>.json` (Claude), daemon `thread/loaded/list` + `thread/read` (Codex).

## 4. Message

Internal normalized record (F12). This is what the receiver chain and the mailbox hold; it is not a wire format. On the A2A ingress it is decoded from the pinned SDK's `Message` (standard `messageId`, `role`, `parts`, `contextId`, `taskId`); CrossTalk fields travel in `metadata` under the `crosstalk.*` namespace and are all optional. A plain text `SendMessage` with no metadata is valid and normalizes to `kind: inform`, `delivery: immediate`.

```
{ "message_id": "<uuidv7>", "in_reply_to": "<message_id>|null", "context_id": "<conversation>", "task_id": "<id>|null",
  "from": "<agent name>", "to": "<agent name>", "kind": "inform|command", "delivery": "immediate|next_turn",
  "hop_chain": ["<24-hex>", ...], "sent_at": "...", "parts": [{"type":"text","text":"..."}] }
```

hop_chain uses Claude's convention (24-hex sha256 prefixes, max 32) so loop guard survives a Claude hop. Forwarding keeps message_id and appends a hop; a reply gets a new message_id and sets in_reply_to.

## 5. Ingress contracts (both feed one receiver)

A2A ingress (required for v1; operator requirement 2026-11-03). Each session is a first-class A2A agent served by its shim.

- Pinned target (F11): **A2A protocol 1.0, JSON-RPC binding**, implemented with the official Go SDK `a2aproject/a2a-go`, pinned to one tagged release in `go.mod`; the conformance client is that release's `examples/helloworld/client`. Method names are the 1.x ones: `SendMessage`, `GetTask`, `CancelTask`. Streaming (`SendStreamingMessage`), push notifications and the extended card are not implemented and are not advertised.
- Agent Card (F13): served at `/.well-known/agent-card.json`, built with the SDK's card type and validated by its resolver. Required fields are filled: `name` (agent name), `description` ("<harness> session, role <role>, cwd <cwd>"), `supportedInterfaces` (one entry: the JSON-RPC URL, protocol version 1.0), `defaultInputModes`/`defaultOutputModes` = `["text/plain"]`, `capabilities` all false, `securitySchemes` = one HTTP bearer scheme and `security` requiring it, `skills` = `inform`, `command`, `ask` each with id, name, description, tags and an example. Role and harness are also repeated under `metadata.crosstalk` for CrossTalk peers.
- Wire handling (F12): the SDK decodes the request; the receiver chain works on the normalized record; the response is the SDK's `Task` or `Message` inside the JSON-RPC success envelope, or a protocol error (`InvalidParams`, `TaskNotFound`, `TaskNotCancelable`, `UnsupportedOperation`), never the internal `{message_id, state}` pair. The A2A encoder and the Codex app-server encoder are separate code: Codex omits the JSON-RPC version header on the wire, A2A includes it.
- Task mapping (F14): every accepted `SendMessage` creates a server-owned task. `inform`/`command`: the task is `submitted` on acceptance, `working` once injected, `completed` when the model's turn containing the injection ends (Claude: next idle status; Codex: turn status `completed`); the task's history holds the delivered message. `ask`: same, but `completed` only when the session calls `reply` for it; the reply text is the task's final `Message` returned by `GetTask`, and `reply` updates that task, it does not merely post a new message to the asker. Acceptance (delivery) and completion are distinct. `configuration.returnImmediately: true` returns the `submitted` task at once (CrossTalk's own `ask` tool always sets it); default returns when the task reaches `working` or a terminal state, bounded by a server timeout that returns the task in its current state. A `SendMessage` with `taskId` continues an existing non-terminal task (its message is appended and injected with the same `context_id`); a terminal or unknown `taskId` is `TaskNotFound`.
- Cancellation (F15): `CancelTask` is phase-dependent. `submitted` (not yet injected): removed from the mailbox, task `canceled`. `working` (already inside the session) or any terminal state: `TaskNotCancelable`; the shim never interrupts the harness turn. A cancel racing a completion is resolved by the task store's single compare-and-set: the first terminal transition wins, a terminal task is never revived. `GetTask` and `CancelTask` are allowed only to the principal that created the task.
- Authentication (F16): bearer tokens are private files, never in descriptors. Each shim holds its own token (0600, path in `credential_ref`) and a per-user shared **peer token** at `~/.local/state/crosstalk/peer.token` (0600, created on first start). Any client presenting the peer token is the principal `local-user`; a shim presenting its own token is the principal of its registered agent. All holders of the peer token are one principal, `local-user`: "own tasks" means every task that principal created, with no isolation between individual non-shim clients. The token is owner-readable only and never appears in descriptors, `list_agents` output, Agent Cards or logs. A principal without agent registration may send `inform` and `ask` and read or cancel its own tasks; `command` requires a registered agent whose role/owner satisfies §7. The conformance test configures the sample client with the peer token.

uds ingress: Claude's two-line protocol on `uds_path`: `{"type":"auth","token":T}` then `{"type":"user","message":{"role":"user","content":C}}`. C may be a `<cross-session-message ...>` envelope. Sender identity = unix peer pid mapped to a registered agent (Claude session pid or shim pid). Unmapped pid → treated as unverified.

Two identities per inbound message (F4): the **transport peer** (the process on the socket: a Claude session pid, another shim, or unknown) and the **original sender** (the registered agent the message is from). On the A2A ingress both are the sending shim's agent. On the uds ingress the transport peer is verified from the socket; the original sender equals it when the peer is a registered agent, and is otherwise unverified. A relay never upgrades the original sender's identity beyond what the transport proves.

Native replies through a shim (F4): when shim B injects a message from A into Claude session B, the envelope's `from` is B's own uds socket and `from-name` is A. Claude's native SendMessage reply therefore arrives at shim B, which must route it to A. Routing uses only shim B's own record of that delivery, keyed by (B's session, the delivered message_id, A); it never derives the destination from the reply text. When more than one original sender has an outstanding delivery into the same Claude session, a native reply with no correlating content is delivered to the shim's mailbox for that session as `in_reply_to: null`, state `uncorrelated`, never guessed (F6).

Native-message normalization (F6): a frame arriving over uds without our metadata is assigned a fresh message_id by the receiving shim, `in_reply_to` stays null unless the correlation above is unambiguous, and it is exempt from the message_id dedupe (identical repeated text is legal). Identified messages: authenticate first, then look up (sender, message_id); acceptance is recorded atomically with the mailbox write so two concurrent duplicates cannot both inject; a retry returns the prior state.

Receiver, both ingresses, in order: authenticate → dedupe on (verified sender, message_id) for identified messages → hop_chain check (repeat or >32 → reject) → policy (§7) → inject (§6). Returns the internal `{message_id, state}`; each ingress encodes that into its own wire response (A2A: Task/error; uds: Claude's native ack).

## 6. Injector per harness

Claude adapter: `inject` = connect to the session's `/tmp/cc-socks/<pid>.sock`, auth with the token from `~/.claude/sessions/<pid>.*.key`, send one `user` line whose content is the reconstructed envelope `<cross-session-message from="uds:<shim uds_path>" from-name="<from>">\n<escaped body>\n</cross-session-message>`. Delivery is mid-turn between tool calls or next turn if idle (harness behaviour; `delivery` is advisory). Verified today.

Codex adapter: `inject(delivery)` over the endpoint recorded in `harness_endpoint` at registration (default: the daemon uds `~/.codex/app-server-control/app-server-control.sock`; websocket framing, JSON-RPC 2.0 without the version header, `initialize` first):
- handshake: `initialize {clientInfo, capabilities:{experimentalApi:true}}`, await the result, then send the `initialized` notification, before any other call (F3).
- immediate: `thread/turns/list` → the turn with status `inProgress` (the `Turn` type has no kind field, so steerability cannot be read from the list) → `turn/steer {threadId, expectedTurnId, input:[{type:"text", text}]}`. Error handling (F1): `activeTurnNotSteerable {turnKind: review|compact}` → fall back to next_turn; expectedTurnId mismatch → refresh the turn list once, retry once, then next_turn. The exact wire placement of these errors (JSON-RPC error.data vs result) is captured from a live capture during build, not assumed. No active turn → `turn/start {threadId, input}` preserving the thread's existing settings (no model/cwd/approval overrides). Verified: steer renders mid-turn in a TUI-attached thread.
- next_turn: v1 uses the `codex queue --remote <harness_endpoint> --thread <threadId> --message <text>` CLI (verified without `--remote` on the default daemon: delivered at the next turn boundary). `--remote` is always passed with the endpoint recorded at registration and the exact thread id, never a name, so steer-then-queue fallback never changes servers (F17); `--resume` is never added. The websocket client for immediate delivery uses the same recorded endpoint. Verified during build against a thread on a non-default server (test 18). The RPC pair `thread/queue/add` → `queuedSubmission` / `thread/queue/start` is present in Codex 0.166.0's `--experimental` schema generation and absent from the default generation (F18); the CLI stays the v1 path until a live test shows add alone wakes idle work and is replay-safe.
- Ambiguous failure (disconnect mid-call, no response) → state `unknown`, no resend; a later reconnect re-reads state and never re-injects.
Text delivered to Codex carries the same envelope as attributed content plus a fixed trusted preamble explaining that peer text has no operator authority.

## 7. Trust and policy

Identity comes from transport (peer pid / A2A auth) matched to registration, never from envelope attributes. Envelope attributes are display only and are reconstructed by the receiver from the verified identity.

| kind | to a subagent | to a primary |
|---|---|---|
| inform | any registered agent; unverified senders allowed | any; unverified allowed |
| command | owner only | any registered primary |
| control | not in v1 | not in v1 |
| unknown kind | rejected | rejected |

Rejected → `state: rejected`, never injected (F7): an unauthorized command is dropped, not downgraded to inform, because the text would carry the same request into the model. A frame over uds that carries no verified command contract is inform regardless of any envelope attribute; attributes cannot promote kind. `command` authorizes work only within the receiving session's existing operator permissions; it never changes approvals or settings. Envelope text nested inside a body is body text and never parsed as a second envelope. Peer text is escaped and labelled; it never enters developer/system instructions. The same-user credential model is accepted as not being isolation.

## 8. Tool surface (identical in both harnesses, served by the shim's MCP server)

- `list_agents()` → registry rows with live status.
- `send_message(to, text, kind="inform", delivery="immediate")` → `{message_id, state}`.
- `reply(message_id, text)` → destination resolved from the stored original, not model-supplied → `{message_id, state}`.
- `ask(to, text, deadline_seconds?)` → `{request_id}` at once; the reply is delivered later as an ordinary inbound message with `in_reply_to`; no blocking.
- `receive_message(after?, limit?)` → mailbox pull, for harnesses/moments where push failed.

States: `accepted | queued | rejected | unreachable | unknown`. None implies read or completed.

## 9. Lifecycle

Shim starts with the session (harness SessionStart hook, or a launch wrapper where herdr is used), binds uds + A2A port, confirms its harness binding, then registers (§3 order), exits with the session and removes only its own-instance descriptor. A crash leaves a file with a dead pid: pruned on read. herdr restore relaunches the harness; the shim relaunches with it and re-registers under the same name.

## 10. v1 scope

In: §3 (without the Claude-visible pair unless R2 says yes), §4, §5 including a full A2A server per session (Agent Card, message/send, tasks/get, tasks/cancel), §6 both adapters, §7 inform+command, §8 all five tools, §9.
Out: control, delegate/spawn, cross-machine, A2A push notifications, `message/stream`.

## 11. Tests that gate "done"

1. Claude→Claude via shim: send, native SendMessage reply, in_reply_to resolved. 
2. Claude→Codex immediate while Codex is mid-turn: rendered before the turn ends.
3. Claude→Codex while idle: new turn starts.
4. Codex→Claude via shim uds: arrives mid-turn with reconstructed envelope and verified sender.
5. Two outstanding asks between the same pair: both replies correlate or the limitation is documented.
6. Duplicate delivery (same message_id twice): second is dropped.
7. hop_chain loop: A→B→A with forwarding is rejected at the repeat.
8. Policy: a non-owner primary sends command to a subagent → rejected, never injected.
9. Unverified uds sender (random pid) sends command → rejected without injection; inform accepted and labelled unverified.
10. herdr restore: after `herdr session attach default`, both agents re-register under the same names and test 2 passes again.
11. Claude-visible record: a shim's `~/.claude/sessions` pair appears in a Claude session's ListAgents, native SendMessage authenticates against the shim's key and the message reaches the shim's receiver, and Claude does not offer it as a resumable or controllable session. Fail → §3 fallback.

12. Binding ambiguity (F2): two codex panes sharing a cwd, plus a third conversation on a different server; each shim delivers only to its bound thread; a shim started without an unambiguous binding refuses to start.
13. Relay routing (F4): A→B via B's shim, B replies natively, reply reaches A, with a third agent C also having messaged B in the same window; C never receives A's reply and vice versa.
14. Forged attributes (F7): envelope claiming from="<owner>" over an unverified transport is rejected for command and reconstructed from the verified identity for inform; a command nested inside envelope body text does not become a command.
15. Replacement vs old exit (F8): start shim2 under the same name while shim1 is alive → shim2 refuses; kill shim1, start shim2, then let a stale shim1 exit path run → shim2's descriptor survives.
16. Injector failure branches (F10): active→idle race during steer (state mismatch → refresh once → queue), idle→active race during turn/start (joins the new turn, acceptable for immediate only), review/compact turn → activeTurnNotSteerable → queue, disconnect right after acceptance → state unknown and no resend, reconnect → no duplicate work. Concurrent duplicate ingress (same message_id on both ingresses at once) → exactly one injection.

17. A2A conformance: the pinned `a2a-go` release's helloworld client, configured with the peer token, resolves a session's Agent Card, calls `SendMessage` with plain text, and the text appears inside that session; `GetTask` shows `working` then `completed`; an `ask` sent with `returnImmediately: true` reaches `completed` with the reply as its final message after the session calls `reply`; `CancelTask` on a `submitted` task cancels it and on a `working` task returns `TaskNotCancelable`.
18. Codex endpoint pinning: a thread on a non-default app-server (`codex --remote <other>`) receives both immediate (steer) and next_turn (queue) delivery from the same shim, and nothing reaches the default daemon.

## 12. Open questions

- R2 answered: conditional yes, gated by test 11.
- A2A auth beyond the same-user bearer/peer-token scheme (needed for cross-machine, out of v1).
- Where the shim is launched from: herdr `agent start` wrapper vs harness SessionStart hook vs both.
- Codex hook integration (F9): Codex `SessionStart` fires on startup, resume, clear, compact and fork, not once per process. The hook must be idempotent per (thread id, shim instance): no second shim after compaction, a fork gets its own binding and never inherits the parent's, and the hook's `session_id` → daemon thread id mapping is verified during build. SessionStart may fire before the model-tool connection is ready; SessionEnd may lag pane closure.


## Changelog
- draft 1 → 1.1 (2026-11-03): applied Codex review findings F1–F10 (all accepted). F1 steer error handling and no Turn.kind; F2 explicit harness binding + ambiguity tests; F3 handshake order, `codex queue` CLI as v1 next_turn, RPC pair noted; F4 transport-peer vs original-sender, native reply routing; F5 credential_ref, redacted discovery; F6 native-message normalization, atomic dedupe, uncorrelated state; F7 unauthorized command rejected not downgraded, unknown kind rejected, nested envelope rule; F8 instance_id, bind-then-publish, own-instance cleanup; F9 Codex hook idempotency; F10 failure-branch tests. Tests 12–16 added. States gained `uncorrelated` (reply mailbox only).


## Review disposition

- Cross-model critique (greenfield gate step 3): performed live by a Codex 0.166.0 session over native messaging on 2026-11-03 (the first use of the mechanism this document specifies). Ten findings returned, all accepted; see the 1.1 changelog. Second pass (same Codex session, on commit 79d52ce, §5/§6): eight findings F11–F18, all accepted; see the 1.3 changelog.
- agent-runner's plan-review workflow was not used: it is retired per the skills repo ADR 0001 (herdr is the agent runtime).

- draft 1.1 → 1.2: moved into the CrossTalk repo as docs/design.md; discovery no longer depends on herdr (ADR 0003); language fixed to Go (ADR 0002); descriptor path renamed to crosstalk; full A2A server per session (Agent Card + tasks) made a v1 requirement, test 17 added.
- draft 1.2 → 1.3: Codex review pass 2 (F11–F18, all accepted): A2A 1.0 + pinned a2a-go SDK; internal record separated from wire format; concrete Agent Card; task state mapping and returnImmediately; phase-dependent cancellation; peer token replaces descriptor-published tokens; Codex `--remote` pinning (test 18); queue RPC availability stated precisely.
