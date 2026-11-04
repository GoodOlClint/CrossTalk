# Change plan: Antigravity CLI harness via hook mailbox

Status: draft for the brownfield gate, revised after the Codex critique (see Review disposition); no implementation until the operator approves. Decision record: [ADR 0012](../decisions/0012-antigravity-harness-via-hook-mailbox.md). Evidence: [spike 12](../spikes/12-antigravity-cli.md).

Terms. **agy** = Google's Antigravity CLI, version 1.5.0 on macOS. **Conversation** = one agy session with a conversation id; agy's `/clear` starts a new one in the same process. **Root id** = `_meta["antigravity.google/root_conversation_id"]`, the top-level conversation behind a tool call. **`_meta`** = the metadata object on an MCP request. **MCP** = Model Context Protocol, how agy calls CrossTalk's tools. **A2A** = Agent-to-Agent protocol, how shims talk to each other. **Shim** = the `crosstalk mcp` process agy starts. **Hook** = a command agy runs at a lifecycle point, reading JSON on stdin and writing JSON on stdout. **`injectSteps`** = the hook output that adds steps to a conversation. **`ephemeralMessage`** = one kind of injected step; agy documents it as a transient system message. **Pointer** = the short fixed sentence a hook injects, containing no peer text. **Mailbox** = messages held in an agent's journal until its model collects them. **Journal** = the per-agent append-only state file. **Marker** = a small file the shim writes and the hook reads. **Credits/quota** = agy's model-usage allowance. **Flash-low** = `gemini-3.8-flash-low`, the cheap model the spike used.

## 1. Goal

An agy session takes part in CrossTalk: Claude, Codex and other agy sessions on the same Mac can send it messages and asks, and it can send and answer through the same seven tools. Delivery uses documented agy interfaces only (MCP, `hooks.json`, `settings.json`).

Binding operator rulings: hook mailbox delivery only; no `agy agentapi`, `agy acp`, `-p` driving, Remote Control scripting or keystrokes; assume a consumer plan; an idle agent does not wake.

## 2. What the operator's father experiences

1. Someone runs `crosstalk install antigravity --name dad` once, with agy closed. It adds entries to three agy config files and leaves everything else in them alone. It prints what it changed.
2. He uses `agy` as usual. In each new conversation, including after `/clear`, the session joins CrossTalk the first time its model calls a CrossTalk tool. He can say "check CrossTalk". Until then no other agent can reach that conversation. Section 5.7 has an optional setting that makes it automatic for one extra model call per conversation.
3. A message from Claude waits quietly. When he next sends a prompt, or the running task reaches its next model call, the model sees a one-line notice he cannot see, calls `receive_message`, and acts on the result. Every model call in a loop triggered this way costs quota. He sees that tool call in the transcript, then the model's reaction. He gets no notification and no pop-up.
4. A message that arrives during a turn is picked up at the next model call. If it arrives after the last model call, it waits for his next prompt by default. With `--stop-delivery` (Q5) the turn gets one continuation instead: at least two more model calls (one to fetch, one to read), and more if the model replies or uses tools. Those calls use his quota.
5. If he leaves agy idle, nothing happens until he types. He can opt in with `/schedule every 15 minutes, call the MCP tool receive_message on server crosstalk`, which spends credits on every firing, even when no mail is waiting.
6. After he closes agy, senders get an error. Nothing queues for a closed session. Unread mail stays in the journal and appears when that conversation is resumed with `/resume` or `agy --conversation`.

## 3. What it will not do

- Wake an idle agent, or interrupt a model call or a running tool.
- Put peer text in a user or system position. The body arrives only as a tool result.
- Reach another machine (non-goal in design §1). **If the father's peers run on the operator's Mac and he does not, this plan does not serve him; see open question Q1.**
- Support the Antigravity App or IDE. They share `~/.gemini/config/` with the CLI, so the shim and hook will run there too, but they stay `unbound` (section 5.2).
- Prove the model read a message. `accepted` means the tool result was handed to agy.
- List agy conversations that have never called a CrossTalk tool. agy has no documented session registry.
- Offer Linux. Peer credentials exist there, but no Linux agy behaviour is measured.

## 4. Current state

| Area | File | Fact relevant here |
|---|---|---|
| Interface | `internal/harness/harness.go:62` | `Harness` has no mailbox or install capability; ADR 0008 says so |
| Sessions | `internal/harness/harness.go:110`, `docs/design.md` §3 Superseded sessions | Optional `harness.Supersession`: true only with proof; a harness without proof ends sessions only with its shim. A superseded descriptor stops resolving at once; the holder retires it every 2 s and before registering a new agent; the journal stays; a resume re-registers and replays; a message to a retired session fails at the sender |
| Model pattern | `internal/harness/codex/harness.go:57-70` | Identity from `_meta` per call; agy copies this shape |
| Chain | `internal/receiver/receiver.go:234,298-310` | The body is durable in the dedupe record before dispatch, so the journal already holds what a mailbox needs; a state `Injectable()` rejects becomes `unknown`; only `rejected` and `unreachable` close the route |
| States | `internal/adapter/adapter.go:22` | `Injectable` lists five states |
| Filter | `internal/receiver/mailbox.go:35,42` | `MailboxStates` excludes `accepted` and `queued`, so `receive_message` cannot return a `queued` message today; `Admitted` (used by receipts) lists `accepted`, `queued`, `unknown` |
| Retention | `internal/journal/journal.go:133` | `unresolved` keeps only named states past 24 h; a new state not added here is truncated and its mail is lost |
| Tasks | `internal/a2a/mapping.go:164` | `outcome` completes an inform on `accepted`/`queued`; any other state stays `working`, including an unlisted new one |
| Late outcomes | `internal/wire/wire.go:178`, `internal/a2a/server.go:381` | `Server.Resolve` exists; the comment says a later outcome "has no receiver hook yet" |
| Reply forwarding | `internal/shim/send.go:551,570` | A reply counts as delivered only on `accepted`/`queued`; otherwise it stays pending and retries |
| Notices | `internal/shim/send.go:324` | Ask-expiry notices pass through `st.Receive`, so they would be mailboxed too |
| Retention dependencies | `internal/journal/journal.go:188,200` | `liveSet` keeps an unanswered outbound ask, and counts an expiry notice as answering it, only on literal `accepted`/`queued` |
| Ask completion | `internal/a2a/tasks.go:457` | `CompleteAsk` rewrites delivery metadata only from `submitted`/`dispatching` |
| Per-call identity | `internal/mcpserver/server.go` `boundTool`, `internal/shim/agent.go:99` | Backends get the `Agent`, not the call; the `RequestContext` on an `Agent` is the registering call's |
| Sweep | `internal/mcpserver/server.go` `Run` | One ticker every 2 s, today only `RetireSuperseded` |
| Direct inject | `receiver.go:499`, `send.go:237` | Bounce and native-target paths call `Inject` with no dedupe record; neither is reachable for agy (no native reply address, empty native rows) |
| Tool | `internal/mcpserver/server.go:543` | `receive_message` is a read-only pull; it has no consume step |
| Server | `internal/mcpserver/server.go:85` | `mcp.NewServer(..., nil)`: no `instructions`, no tool `_meta` |
| Entry | `cmd/crosstalk/main.go:39-48,54-66` | Subcommands `mcp`, `capture`; harness list `{cx, cl}` |
| Version gate | `internal/harness/codex/version.go` | Pattern: tested majors refuse, untested minors warn |
| Peer notice | `internal/harness/codex/text.go:10` | Fixed "no operator authority" text exists for Codex only |

## 5. Design

### 5.1 Trust: pointer in the hook, body in a tool result

ADR 0007's reason holds: peer text delivered as user input looks like operator text. The hook injects only this template, with an integer from a validated marker:

`CrossTalk: N message(s) waiting. Call the MCP tool receive_message on server crosstalk.`

The wording names the server because the spike's first prompt, which named only the tool, made the model use its shell, and the second, which said "MCP tool ... server", worked. No sender name, id, kind or body goes in the pointer: names can be chosen by a model and are peer-influenced.

`ephemeralMessage` is the right step. It is measured to reach the model as `source: SYSTEM_SDK, type: EPHEMERAL_MESSAGE` and is hidden in the TUI (agy's terminal interface). `userMessage` renders as a line the father appears to have typed. `systemMessage` carries the strongest label and was not run. `toolCall` has an unknown schema, and the attempt turned the whole turn into `ERROR`. The pointer is re-injected at each model call while mail is waiting, so a model that ignores it once costs nothing extra.

`receive_message` results gain a fixed top-level `notice`, moved out of the Codex package: the bodies are peer text with no operator authority. A tool result is a lower-authority position, but prompt injection remains possible. Whatever a model does with it stays inside agy's own permission prompts.

### 5.2 Identity and binding (adapter)

`Identify` claims a call only when `_meta` has the root id or conversation id and `antigravity.google/artifacts_dir` contains `/antigravity-cli/`. This keeps App and IDE conversations `unbound`. `session_ref` is the root id, falling back to the conversation id. A subagent's call acts as its root (the changelog gives this as the field's purpose; not yet measured), with one guard: `Session.Nested` is true when the two ids differ. Nesting travels per call in the request context, because the `Agent` holds only its registering call. A nested `receive_message` omits every `mailboxed` and `collecting` entry, and a nested call counts as no settlement evidence (5.3), so a subagent can neither read nor take its parent's mail. `reply` to a message still `mailboxed`, from any caller, returns `not_collected` and changes nothing, so an ask cannot be completed before its recipient has seen it. Missing `_meta` means `unbound`, as for Codex: it fails closed.

`HarnessPID` is the parent pid, measured as agy. One process holds one agent record per root id, the same shape as Codex pooling. Name: `CROSSTALK_NAME` from the MCP entry's `env` if set and free, otherwise the existing fallback `antigravity-<cwd basename>-<8 hex>`. agy has no session name to read.

### 5.3 Delivery state: add `mailboxed`; do not reuse `queued`

`queued` is defined as "verified no-wake handoff to Codex model history", §6 says "shim mailbox placement alone is insufficient", `MailboxStates` excludes it, and `outcome` completes the sender's task on it. Reusing it would redefine a state and report an unread message as delivered.

`mailboxed` = durable in the recipient's journal, reply route open, ask still open, not yet handed to the model. The chain is unchanged: authenticate → dedupe → hop-chain → policy → `Inject`.

**Disagreement with the steer: `Inject` writes no mailbox.** The journal already holds the body, so a second store would be a parallel implementation with its own crash windows. The adapter's `Inject` validates the target and returns `mailboxed`. The receiver's existing `Resolve` and `SetDedupeState` calls make it durable, and only then is the marker updated (5.5). A crash between claim and outcome becomes `unknown` through the existing `Recover`, stays in the journal and appears in `receive_message`, though no pointer announces it (gap G4).

Collection is a transaction with an acknowledgment boundary, so delivery is never reported before evidence of handoff:

1. **Select and mark.** A non-nested `receive_message` runs `Receiver.Collect(after, limit)` under a per-agent mail lock. It selects `mailboxed` entries up to `limit` and a 128 KiB total (at least one) and durably sets each to `collecting`, a journal-internal state like `dispatching`. It also re-stamps inform routes. The sender's task does not change. Selection and marking share the lock and check the latest state, so a second collector finds nothing to select.
2. **Return.** The bodies go back as the tool result.
3. **Settle on evidence.** A `collecting` entry becomes `accepted` (dedupe and outcome), and its inform task completes, on the first evidence that agy's loop continued past that tool call: the conversation's next `PreInvocation` hook firing, read from the `seen` marker with a time after the `collecting` record, or the next non-nested bound call from that conversation. The shim checks on each bound call and on the existing 2 s tick. A model call cannot start in that conversation while its tool call is in flight, so this evidence is specific to the conversation.
4. **Ambiguity stays ambiguous.** A `collecting` entry with no evidence when the agent exits, or found on `Recover`, becomes `unknown`, as ADR 0007 prescribes. The task stays `working` and the body stays readable through `receive_message`. Nothing is re-injected. If the result did reach the model, the model may see it again labelled `unknown`, which is a duplicate display and never a false completion.

Remaining limit, stated: settlement proves agy continued, not that the model used the content. A user interrupt followed by a later prompt can settle a result agy dropped; test L11 records what happens.

Sender's task: a `mailboxed` inform stays `working` and completes at settlement, so `completed` keeps its meaning of harness-side evidence. Ask stays `working` until `reply`, as now. `send_message` returns `mailboxed` with a `hint` (`Result.Hint`, `internal/mcpserver/backend.go:32`) saying the recipient reads it at its next model call and may be idle. A third-party `SendMessage` in default-wait mode blocks until settlement or disconnect, which is §5's rule as written. An ask whose deadline passes while mailboxed is still collected, and `reply` returns `ask_closed` (ADR 0010). `CancelTask` on a mailboxed message returns `TaskNotCancelable`, since the claim is already taken. Back-pressure is the existing pending cap (256 unresolved tasks); native frames stay unbounded per WA2.

Route lifetime: ADR 0010's one-hour inform expiry should start when the model sees the message. `Collect` journals a fresh open record with the current time, and re-adds the route if the sweep closed it.

### 5.4 Core changes, separate from the adapter

| # | File | Change |
|---|---|---|
| 1 | `internal/adapter/adapter.go` | Add `Mailboxed`, `Antigravity`; `Injectable` accepts `Mailboxed` |
| 2 | `internal/journal/journal.go` | `StateMailboxed`, `StateCollecting`; `unresolved` keeps both for dedupe and outcome records; `liveSet:188` counts an expiry notice in these states as sent; `liveSet:200` keeps an unanswered outbound ask whose state is `mailboxed` |
| 3 | `internal/receiver/mailbox.go` | `MailboxStates` adds `mailboxed` only for the collect path; `Admitted` includes both; add `Collect`, `Settle`, `MailboxedCount`; per-agent mail lock; `Recover` turns `collecting` into `unknown` |
| 4 | `internal/receiver/receiver.go` | `Config.OnSettled`, `Config.OnMailChanged`; the final state write and count publication for a `mailboxed` outcome run under the mail lock; line 500 treats `Mailboxed` as success |
| 5 | `internal/a2a/mapping.go`, `tasks.go`, `internal/wire/wire.go` | `outcome` names `Mailboxed` as working; `tasks.go:457` also rewrites `mailboxed` delivery metadata on completion; wire `OnSettled` to `Server.Resolve(id, Accepted, "")`; `:331` no reason for it |
| 6 | `internal/shim/send.go` | Lines 551 and 570 count `Mailboxed` as the forward's evidence; `Mailbox` calls `Collect`, or returns no mailboxed entries when nested; `Reply` returns `not_collected` for a `mailboxed` message; bound call and tick call `Settle` |
| 7 | `internal/harness/harness.go` | `Session.Nested`; optional `MailSignal`, `ToolAnnotator` |
| 8 | `internal/mcpserver/` | Per-call nesting in the context; `notice` on results; tool `_meta` from `ToolAnnotator`; `instructions`; tool description; list row status; tick runs `Settle` and marker repair |

New optional interfaces: `MailSignal { MailChanged(s Session, seq uint64, pending int) error; SeenSince(s Session, t time.Time) bool }`, whose first method is called under the mail lock after the journal records a change, and `ToolAnnotator { ToolMeta(tool string) map[string]any }`, which adds harness-defined `_meta` to `tools/list` entries.

### 5.5 Hook program: `crosstalk hook antigravity <event>` (adapter)

Stateless, one process per firing, no daemon, never opens the journal. It reads stdin (64 KiB cap), derives `agent_id` from `conversationId` with the existing hash, and reads `~/.local/state/crosstalk/antigravity/mail/<agent_id>.json`, written atomically by the shim as `{"seq":7,"n":3,"pid":…,"started_at":"…"}`. The count is recomputed and written under the mail lock with a rising `seq`, so a delayed zero cannot overwrite a newer arrival. A failed write sets a dirty flag and the 2 s tick repairs it; each registration rewrites it. It emits a pointer only if `n` is an integer from 1 to 9999 and that pid is alive with that start time, so a crashed shim cannot cause a pointer-driven retry loop. A subagent's own id has no marker, so it gets no pointer. The shim removes the marker when the agent exits or retires, and rewrites it on registration.

| Event | Output with mail | Otherwise |
|---|---|---|
| `pre-invocation` | `{"injectSteps":[{"ephemeralMessage":"<pointer>"}]}` | `{}` |
| `stop` (installed only with `--stop-delivery`) | `{"decision":"continue","reason":"<pointer>"}` when `executionNum` is 0, `error` is empty, `terminationReason` is not `ERROR`, `fullyIdle` is true | `{}` |

**Fail open.** The spike showed a malformed step errors the whole turn, so the only outputs are these literals, pinned by golden tests. Any panic, parse error or missing file gives `{}` and exit 0. An internal 150 ms timer prints `{}` and exits; `hooks.json` sets `timeout: 2`, the smallest safe whole-second value. What agy does on a missing binary, non-zero exit or timeout is unmeasured (spike S3).

**Stop guard, and what it does not bound.** `executionNum` 0 gives at most one `Stop` continuation per turn. It does not bound model calls: the continuation needs at least one call to request `receive_message` and one to read the result, and replies, other tools or further pointers add more, so peer exchanges can prolong that loop. The spike's continuation used no tools, so it is no cost measurement. `Stop` delivery is therefore a separate opt-in commit, measured in S7 before anyone enables it; without it, mail waits for the next prompt or model call. The transcript shows the `reason` as a `SYSTEM_MESSAGE` block, which is another reason to keep it fixed. Whether `executionNum` resets on the next prompt in the same conversation is unproven; if it does not, the guard fails safe to one continuation per conversation.

The `pre-invocation` hook also touches `antigravity/seen/<agent_id>`, which is the settlement evidence in 5.3, proves hooks run, and gives "last active" for `list_agents` and `--check`. That is the only write. If hooks do not run, settlement falls back to the next bound call.

### 5.6 Sessions, `/clear` and names

v1 follows the rule: without proof, do not retire. After `/clear`, the old conversation's record stays live until agy exits, holding its name. The new conversation registers at its first tool call and gets the fallback name. **Top known gap (G1): a sender using the old name gets `mailboxed` for a conversation nobody is in.** `list_agents` shows both rows with last-active times, and the `hint` says the recipient may be idle, but this is still silent stranding.

**Disagreement with the steer: a current-conversation source can exist.** The shim sees every call's root id, so each agy process has a "latest root" the shim can record in `antigravity/proc/<agy pid>.current`. `Superseded(s)` then reads that file, as Claude's reads its session file. Subagents share the root, so they do not trigger it. This is proof only if an agy process runs one root conversation at a time. Spike S2 tests that (`/fork`, `/resume`, a scheduled wake-up, a background subagent). If S2 passes, commit 11 implements `harness.Supersession` and design §3's readers, retirement and name rules apply unchanged: the old name stops resolving as soon as the new root is recorded, the retire sweep removes the old listeners and marker within 2 s, and `CROSSTALK_NAME` moves to the new agent through the existing per-call name retry. agy has no native row, so unlike Claude the replacement is not listed before its first call. If it fails, or the App shares a process, G1 stays and is reported to the operator. Even with S2, the name moves at the new conversation's first tool call, not at `/clear`. Unread mail stays with the old conversation (§5's rule against routing records into another conversation) and returns on `/resume`.

### 5.7 Discovery and first contact

v1: descriptors only; no `Discover` rows for unbound conversations. A Claude cannot reach an agy conversation until it has called a CrossTalk tool once. This is acceptable for one person who can say "check CrossTalk", and unacceptable if agy is expected to be reachable from cold, so the operator should choose (Q2).

Option offered: `--announce` at install. When `invocationNum` is 0 and no descriptor exists for this `agent_id`, the hook injects a fixed line asking the model to call `list_agents` once. It uses the measured event, needs about 20 lines and costs one model call per conversation. The steer's SessionStart idea has the same cost on an event the spike did not exercise.

Rejected for v1: having the shim register from a hook signal with no tool call. It needs a hook-to-shim path, proof of which agy process fired the hook (unmeasured), and agent creation with no `RequestContext`. Revisit if `--announce` proves too costly.

### 5.8 Install

`crosstalk install antigravity [--name N] [--announce] [--stop-delivery] [--no-allow]`, `uninstall antigravity`, `install antigravity --check`. Documented manual steps are not enough for a non-developer editing three JSON files. Claude and Codex keep their install lines. All code lives in the adapter package; `cmd/crosstalk` dispatches, as it is the sole harness importer.

| File | Key owned by CrossTalk | Value |
|---|---|---|
| `~/.gemini/config/mcp_config.json` | `mcpServers.crosstalk` | absolute binary path, `args:["mcp"]`, optional `env.CROSSTALK_NAME` |
| `~/.gemini/config/hooks.json` | top-level `crosstalk` | `PreInvocation`, plus `Stop` only with `--stop-delivery`; absolute path, `timeout:2` |
| `~/.gemini/antigravity-cli/settings.json` | one string in `permissions.allow` | `mcp(crosstalk/*)` |

Rules: an empty file is `{}` (the operator's `mcp_config.json` is 0 bytes today); unparseable JSON aborts with no writes to any file; unknown keys are preserved; temp file plus rename, keeping mode; a `.crosstalk.bak` copy once per file; idempotent. An existing `mcpServers.crosstalk` or `hooks.crosstalk` whose value differs from what install would write aborts with no writes to any file, and names the conflict. Install records in `~/.local/state/crosstalk/antigravity/install.json` each entry it added and its exact value; a permission string that was already present is recorded as not added. Uninstall removes only recorded entries whose current value still matches, reports any it leaves because the user edited them, and never restores the backup over later edits. The `.bak` is for manual recovery only. It refuses to run while an `agy` process is live, since agy rewrites `settings.json` itself. The allow rule matches Codex's approve-mode line and means a model can message peers without a prompt; `--no-allow` opts out at the price of a prompt on each tool call. `--check` prints each entry, `agy --version`, tested-or-not, and whether any `seen` marker exists.

### 5.9 Teaching the model

Choose the smallest layer that passes this test on flash-low in fresh conversations: "tell <peer> hello" uses CrossTalk `send_message`, and the pointer leads to `receive_message`. Try in order and stop at the first pass:

1. `_meta: {"antigravity/alwaysLoad": true}` on all seven tools (changelog: the agent gets the tool directly instead of discovering it) plus clear descriptions. Expected to be enough for the pointer.
2. Add MCP `instructions`. It is not known whether agy surfaces them.
3. Add `~/.gemini/config/rules/crosstalk.md` (a fourth install file; loaded in every conversation).

Note the name clash: agy has a built-in `send_message` tool for its own agents, so "tell Claude" could go there. Test it explicitly (S4). A skill is rejected: it must be discovered first, which is the failure being fixed.

### 5.10 Version gate

As ADR 0006 and `codex/version.go`: at `Bind`, once per process, run the parent process's executable with `--version` (2 s limit; a read-only one-shot, not driving). `TestedMajors {1}` refuses, `TestedMinors {"1.5"}` warns; the warning goes in `--check`, the descriptor's `harness_version` and a `notice` on the first tool result. agy's changelog moves quickly, so behaviour guards matter more than the number: missing `_meta` keys → `unbound`; missing hooks → no `seen` marker, shown by `--check` and in the first tool result.

## 6. Sequencing

Prerequisites: spike S1 passed (or the install target changed); ADR 0012 accepted. Each commit is `go test ./...` green, carries `Provisional-ADR: 0012`, and starts from the failing test named.

| # | Commit | Test that fails before |
|---|---|---|
| 1 | Add `mailboxed`, `collecting`; retention keeps them | Fake adapter returns it: state stays `mailboxed` not `unknown`; route open; record survives a 25 h replay; an unanswered outbound ask at `mailboxed` survives retention, its deadline re-arms after restart, and a delayed reply's receipt still validates (extends `TestRetentionKeepsUnansweredOutboundAsk`) |
| 2 | `Receiver.Collect`, `Settle` | Barrier test: two concurrent collectors and an arrival return each message once; `Mailbox` does not consume; Claude and Codex entries never flip; stale inform route usable after collect at +2 h; a crash injected between every pair of transitions (mark, return, settle) leaves `mailboxed`, `unknown` or `accepted`, never a completed task without evidence, and the body stays readable |
| 3 | A2A and reply evidence | Inform task `working` until settle, then `completed`; no completion after disconnect before settle; ask stays `working`; `reply` to `mailboxed` is `not_collected`; reply to a mailboxed asker is `delivered` with no retry after restart; ask completion rewrites delivery metadata; `reconcile` after restart |
| 4 | `MailSignal` | Marker equals journal count after accept, collect, settle, `Recover`; barrier test: delayed zero from a collector cannot overwrite a newer arrival; failed write leaves state durable and the tick repairs it |
| 5 | `receive_message`: per-call nesting, `notice`, budget, tool `_meta`, `instructions` | Parent-first, child-first and concurrent calls: a nested call never sees parent mail, even with one `Agent` shared; nested call is not settlement evidence; 300 KiB backlog returns under budget and the rest stays; Claude `tools/list` has no agy `_meta` |
| 6 | Adapter package, one line in `cmd` | Identify table (root, conversation only, App path, none); pooled conversations never cross-bind; `arch_test` still passes with no core import |
| 7 | `crosstalk hook` `pre-invocation` | Golden outputs; marker with dead pid or `"n":"x"` or a body field gives `{}`; hostile stdin gives `{}`; 150 ms bound with a stalled read; `seen` touched only with a valid `conversationId` |
| 7a | `crosstalk hook` `stop`, `--stop-delivery` | Stop guard table; not installed by default |
| 8 | Install, uninstall, `--check` | Fixture: existing hooks and servers survive; 0-byte file; bad JSON writes nothing; install twice is byte-identical; conflicting existing `crosstalk` entry aborts with no writes; pre-existing permission string survives uninstall; user edit between install and uninstall is left and reported |
| 9 | Version gate | Major 2 refused, 1.6 warns, unparseable handled |
| 10 | Docs: design, CLAUDE.md, ADRs (section 10) | None: doc only |
| 11 | Conditional on S2: `Superseded` from recorded root | Design test 32's cases for agy; subagent call does not supersede |
| 12 | Conditional on Q2: `--announce` | Fires only at `invocationNum` 0 with no descriptor |
| 13 | Live definition of done (section 7) recorded in `docs/spikes/` | The run itself |

## 7. Test bar

Offline, no quota: `fakeagy`, a test MCP client sending the measured `_meta` and `initialize` frames, plus hook stdin fixtures copied from spike 12. It drives a real shim and real `crosstalk hook` in `go test`, with A2A between two shims, including restart. Design §11 gains tests 34+ for these rows.

Live definition of done, real agy with real Claude and Codex on the Mac. Pass means the transcript and journal agree.

| L | Scenario | Pass | Prompts |
|---|---|---|---|
| 1 | Install on a machine with existing config | `/hooks` and `/mcp` list CrossTalk; other entries intact | 0 |
| 2 | Claude → idle agy inform | Sender sees `mailboxed`; agy stays idle (hook log silent, no model call); next prompt: `receive_message`, body in a tool result, TUI shows no peer text as user input | 1 |
| 3 | Codex → agy during a long tool call | Pointer at the next model call, not before | 1 |
| 4 | Default: arrival after last model call waits for next prompt. With `--stop-delivery`: one continuation, no second; record actual model calls, plus a reply loop | Counts match S7 | 2 |
| 5 | Claude asks agy; agy `reply`; asker receives with correct `in_reply_to` | Task `completed` with reply text | 1 |
| 6 | agy asks Codex; reply lands mailboxed; collected at next prompt | Round trip complete | 2 |
| 7 | agy → agy | Both ends | 2 |
| 8 | Restart with mail waiting: quit, send fails at sender; `agy --conversation <id>` | Earlier unread mail collected; open ask still answerable | 1 |
| 9 | `/clear` with mail waiting | New conversation has no old mail; behaviour matches 5.6 (gap or commit 11); `/resume` recovers mail | 2 |
| 10 | Remove the binary mid-session | Turn still completes, or the behaviour is recorded and blocks release | 1 |
| 11 | Interrupt during `receive_message`, then prompt again | Task and transcript recorded; no completion if agy dropped the result, or the limit in 5.3 is confirmed and named | 2 |

Quota: about 15 prompts on flash-low with one-sentence prompts; check `/usage` before and after; stop and report if a scenario needs more than twice its budget; repeat only failed rows. Everything testable with `fakeagy` is tested there first. `/schedule` is tested once at most, and only if the operator wants it verified, because each firing costs.

## 8. Rollout

Operator's Mac first, through L1–L11, with the terms question decided. Then the father's Mac: install the same binary, run `--check`, one live inform and one ask with him present, and walk him through section 2 and the terms risk. Rollback is `crosstalk uninstall antigravity`: it removes what install added and leaves everything else, including later edits; journals remain. Pure-additive for Claude and Codex: no behaviour changes unless a harness returns `mailboxed`.

## 9. Risks

| # | Risk | Treatment |
|---|---|---|
| 1 | **Terms.** Google's Antigravity terms say: "using the Service in connection with products not provided by us" and "Using third party software, tools, or services to access the Service (e.g. using OpenClaw with Antigravity OAuth) is a breach ... may be grounds for suspension or termination of your Antigravity and/or Gemini CLI accounts." | CrossTalk touches no OAuth, makes no call to Google and uses only documented MCP and hook interfaces; every model call is agy's own. **Whether a documented MCP or hook setup counts as third-party access is not Google's stated position.** The first sentence read literally is broad; the docs invite third-party MCP servers. The risk falls on the father's account. Operator decides, father is told before install; asking at the linked forum is an option. Enterprise plans may differ; assumed consumer |
| 2 | **`_meta` keys are in the changelog, not the docs**, and agy releases often; hook behaviour already differs from its docs (spike 12) | Identity fails closed to `unbound`; version warning and `--check`; `fakeagy` fixtures pinned to spike frames; re-verify per minor as ADR 0006 does for Codex |
| 3 | **Global hooks not read by the CLI** (docs list it; a secondary source says plugins only; only the workspace file is measured) | S1, 0 prompts, before commit 1. Fallback: packaged plugin, then workspace file per project, with a plan amendment |
| 4 | Stranded mail after `/clear` (G1) | Section 5.6; known and stated; may be top user-visible failure |
| 5 | Model ignores pointer or uses shell | Re-injected each model call; 5.9 test; a smaller model is the worst case and is what the tests use |
| 6 | Hook failure breaks a turn | Literal outputs, golden tests, 150 ms bound, S3, L10 |
| 7 | Prompt injection through a peer's body | Tool-result position, `notice`, existing permission prompts; not eliminated; same-user trust model per ADR 0004 |
| 8 | Credit spend the father did not expect | A pointer-driven loop costs at least two model calls and has no enforced ceiling. `Stop` delivery, `/schedule` and `--announce` are all opt-in; S7 measures cost; section 2 says so |
| 9 | Shared config reaches App and IDE | Stay `unbound`; hooks exit at once because no marker exists. Confirm in S5 |

Known gaps shipped: G1 stranded name after `/clear` unless commit 11 lands. G2 no reach before first tool call unless `--announce`. G3 an interrupted collection can show a message again labelled `unknown`, and settlement cannot prove the model used it. G4 a crash-window `unknown` message has no pointer; it appears on any `receive_message`.

## 10. Existing text that must change (commit 10; not edited yet)

| Document | Change |
|---|---|
| ADR 0003 | agy has no registry: not every running session is listed |
| ADR 0006 | Identity list and install line gain agy; note that agy runs `crosstalk hook` and the shim runs one `--version`; neither wraps or spawns a session |
| ADR 0008 | Mailbox capability is now optional `MailSignal`; install and hook entry are additional `cmd` lines; "one package plus one line" becomes three lines for agy |
| ADR 0010 | Inform route expiry for a mailboxed message starts at collection |
| ADR 0007 | No change; ADR 0012 cites it. Its rejected "receiver mailbox" was scoped to Codex |
| `docs/design.md` | Terms; §2; §3 (identity, name, descriptor `harness`); §5 (retention list, task mapping); §6 new adapter; §8 (`receive_message` no longer read-only for `mailboxed`; states list; `accepted` definition; `reply` reason `not_collected`); §3 Superseded sessions and §9 Exit (`/clear` takes over the name only where a harness proves supersession); §9 install and "SessionStart hooks are v1.1"; §10 out-list wording on hooks; §11 tests; §12 ADR 0008 note and next harnesses |
| `CLAUDE.md` | Architecture, adapters, trust paragraph; back-link ADR 0012; install command |

## 11. Open questions

For the operator:

- **Q1. Where are the father's peers?** Cross-machine is out of v1. If he has no Claude or Codex on his own Mac, only agy-to-agy works for him. Decide before building.
- **Q2.** Is the first-tool-call rule acceptable, or should `--announce` be on, at one model call per conversation?
- **Q3.** Terms risk (risk 1): proceed, tell the father, and ask Google or not?
- **Q4.** Give up on the `/clear` name if S2 fails, or block release on it?
- **Q5.** After S7 measures the cost, should `--stop-delivery` be recommended? It is off by default in this plan, so a message arriving at the very end of a turn waits for the next prompt.

Further small spikes, each before the commit that depends on it. Total about 9 prompts on flash-low.

| S | Question | Method | Prompts |
|---|---|---|---|
| S1 | Does the CLI read global `hooks.json` and global MCP config? | Put a logging hook in the global file, open agy, `/hooks` and check `agy.log` ("loaded N named hooks") | 0, plus 1 to see it fire |
| S2 | One root per process? | `/fork`, `/resume`, `/schedule`, and a second `agy` (a second process): compare pid and root id on tool calls | 2 |
| S3 | Hook failure modes | PreInvocation with missing binary, exit 1, sleep past timeout; log hook's parent pid | 2 |
| S4 | Tool layer and `send_message` clash | Section 5.9 ladder | 2 |
| S5 | Subagent | Root id vs conversation id on a subagent's tool call; whether hooks fire and with which id; App config check, no prompt | 1 |
| S6 | Mid-turn PreInvocation | Marker written during `sleep 60`; consumed at next model call; `executionNum` on next prompt | 1 |
| S7 | `Stop` delivery cost | With a real tool call in the continuation, count model calls to fetch and to fetch then reply | 2 |

## 12. Size

About 6 working days of implementation and live testing: core 2.5 (commits 1–5, including the collection transaction and barrier tests), adapter and hook 1 (6–7a), install 1 (8, with the conflict rule and install record), gate and docs 0.5 (9–10), spikes and live run 1, conditional commits 11–12 up to 0.5. Add a day if S1 forces the plugin route. The critique added about 1 day.

## Review disposition

Codex adversarial review of commit 9b165b6, read-only over the branch diff and connected source (no runtime tests, no live agy). Verdict: needs-attention. Six findings, stated as Codex stated them; all six accepted after checking each against the code. Severity is Codex's. Log: `/tmp/crosstalk-agy-plan-codex.log`.

| # | Sev | Finding | Disposition | What changed |
|---|---|---|---|---|
| 1 | high | "Collection reports delivery before handing off the result." The order marks messages accepted and completes inform tasks before returning the tool response; a crash or disconnected client can leave the sender seeing completed while the recipient received nothing, and later `receive_message` calls exclude the entry. "Naming this G3 does not preserve the delivery-evidence contract." | Accepted | 5.3 is now a transaction: `collecting`, return, settle on evidence, `unknown` on ambiguity. ADR decision 3 rewritten. Commits 2–3 test a crash between every transition; L11 added |
| 2 | high | "Retention changes omit unanswered outbound asks." `journal.go:200` retains them only when `accepted` or `queued`; after a sender restarts past 24 h an unanswered ask can vanish while the recipient still holds it, losing deadline restoration and receipt validation. | Accepted; confirmed at `journal.go:200`. A sweep of readers also found `journal.go:188` and `a2a/tasks.go:457` | 5.4 rows 2 and 5; section 4; commit 1 test |
| 3 | high | "Nested callers can read and answer the parent's mailbox." The nested branch calls `Mailbox`, which would return bodies and ids once `mailboxed` is in the filter; a nested caller can then `reply`, completing the ask before the parent collects. | Accepted | 5.2: per-call nesting, nested results omit parent mail, `reply` to `mailboxed` returns `not_collected`. ADR decision 6; commits 3 and 5 |
| 4 | high | "Specify atomic collection and ordered marker publication." `Mailbox` snapshots and writes lock individually, so concurrent collectors can return the same message; a delayed zero marker can overwrite a newer arrival, leaving mail unannounced. | Accepted | 5.3 mail lock; 5.5 rising `seq` and repair on tick; barrier tests in commits 2 and 4 |
| 5 | medium | "Uninstall cannot restore pre-existing owned keys." Install followed by uninstall destroys a manually configured `mcpServers.crosstalk` or an existing permission string; a once-only backup does not restore prior content as rollout promises, and restoring it would discard later edits. | Accepted | 5.8: conflict aborts, install record, uninstall removes only unchanged recorded entries. Section 8 no longer promises restoration. ADR decision 9; commit 8 |
| 6 | medium | "One Stop continuation does not bound model calls." `executionNum` limits re-entry, not the loop it starts: at least one call to request `receive_message` and one to process it, plus replies and tools; the spike's continuation replied without tools, so it establishes no cost bound. | Accepted | Claim corrected in section 2, 5.5, risk 8 and ADR decision 5. `Stop` delivery is opt-in (`--stop-delivery`, commit 7a); S7 measures it; Q5 added |

Rejected: none. Codex suggested an enforceable invocation budget as one option under finding 6. The plan takes the other branch, default off and measured, because a stateless hook cannot count a loop and Q5 leaves the choice with the operator.

New decision forced by finding 1: settlement evidence relies on hook or bound-call activity, not on a transport write. A wrapper around the MCP transport's write was considered; a successful pipe write proves less than agy's loop continuing, and it needs request-id plumbing through the SDK.
