# Change plan: Antigravity CLI harness via hook mailbox

Status: draft for the brownfield gate; no implementation until the operator approves. Decision record: [ADR 0012](../decisions/0012-antigravity-harness-via-hook-mailbox.md). Evidence: [spike 12](../spikes/12-antigravity-cli.md).

Terms. **agy** = Google's Antigravity CLI, version 1.5.0 on macOS. **Conversation** = one agy session with a conversation id; agy's `/clear` starts a new one in the same process. **Root id** = `_meta["antigravity.google/root_conversation_id"]`, the top-level conversation behind a tool call. **`_meta`** = the metadata object on an MCP request. **MCP** = Model Context Protocol, how agy calls CrossTalk's tools. **A2A** = Agent-to-Agent protocol, how shims talk to each other. **Shim** = the `crosstalk mcp` process agy starts. **Hook** = a command agy runs at a lifecycle point, reading JSON on stdin and writing JSON on stdout. **`injectSteps`** = the hook output that adds steps to a conversation. **`ephemeralMessage`** = one kind of injected step; agy documents it as a transient system message. **Pointer** = the short fixed sentence a hook injects, containing no peer text. **Mailbox** = messages held in an agent's journal until its model collects them. **Journal** = the per-agent append-only state file. **Marker** = a small file the shim writes and the hook reads. **Credits/quota** = agy's model-usage allowance. **Flash-low** = `gemini-3.8-flash-low`, the cheap model the spike used.

## 1. Goal

An agy session takes part in CrossTalk: Claude, Codex and other agy sessions on the same Mac can send it messages and asks, and it can send and answer through the same seven tools. Delivery uses documented agy interfaces only (MCP, `hooks.json`, `settings.json`).

Binding operator rulings: hook mailbox delivery only; no `agy agentapi`, `agy acp`, `-p` driving, Remote Control scripting or keystrokes; assume a consumer plan; an idle agent does not wake.

## 2. What the operator's father experiences

1. Someone runs `crosstalk install antigravity --name dad` once, with agy closed. It adds entries to three agy config files and leaves everything else in them alone. It prints what it changed.
2. He uses `agy` as usual. In each new conversation, including after `/clear`, the session joins CrossTalk the first time its model calls a CrossTalk tool. He can say "check CrossTalk". Until then no other agent can reach that conversation. Section 5.7 has an optional setting that makes it automatic for one extra model call per conversation.
3. A message from Claude waits quietly. When he next sends a prompt, or the running task reaches its next model call, the model sees a one-line notice he cannot see, calls `receive_message`, and acts on the result. He sees that tool call in the transcript, then the model's reaction. He gets no notification and no pop-up.
4. A message that arrives during a turn is picked up at the next model call. If it arrives after the last model call, the turn gets one extra model call before it ends. That call uses his quota.
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
| Reply forwarding | `internal/shim/send.go:529,548` | A reply counts as delivered only on `accepted`/`queued`; otherwise it stays pending and retries |
| Notices | `internal/shim/send.go:306` | Ask-expiry notices pass through `st.Receive`, so they would be mailboxed too |
| Direct inject | `receiver.go:499`, `send.go:219` | Bounce and native-target paths call `Inject` with no dedupe record; neither is reachable for agy (no native reply address, empty native rows) |
| Tool | `internal/mcpserver/server.go:437` | `receive_message` is a read-only pull; it has no consume step |
| Server | `internal/mcpserver/server.go:67` | `mcp.NewServer(..., nil)`: no `instructions`, no tool `_meta` |
| Entry | `cmd/crosstalk/main.go:38-48,55-66` | Subcommands `mcp`, `capture`; harness list `{cx, cl}` |
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

`Identify` claims a call only when `_meta` has the root id or conversation id and `antigravity.google/artifacts_dir` contains `/antigravity-cli/`. This keeps App and IDE conversations `unbound`. `session_ref` is the root id, falling back to the conversation id. A subagent's call acts as its root (the changelog gives this as the field's purpose; not yet measured), with one guard: `Session.Nested` is true when the two ids differ, and a nested call never collects mail, so a subagent cannot take its parent's messages. Missing `_meta` means `unbound`, as for Codex: it fails closed.

`HarnessPID` is the parent pid, measured as agy. One process holds one agent record per root id, the same shape as Codex pooling. Name: `CROSSTALK_NAME` from the MCP entry's `env` if set and free, otherwise the existing fallback `antigravity-<cwd basename>-<8 hex>`. agy has no session name to read.

### 5.3 Delivery state: add `mailboxed`; do not reuse `queued`

`queued` is defined as "verified no-wake handoff to Codex model history", §6 says "shim mailbox placement alone is insufficient", `MailboxStates` excludes it, and `outcome` completes the sender's task on it. Reusing it would redefine a state and report an unread message as delivered.

`mailboxed` = durable in the recipient's journal, reply route open, ask still open, not yet handed to the model. The chain is unchanged: authenticate → dedupe → hop-chain → policy → `Inject`.

**Disagreement with the steer: `Inject` writes no mailbox.** The journal already holds the body, so a second store would be a parallel implementation with its own crash windows. The adapter's `Inject` validates the target and returns `mailboxed`. The receiver's existing `Resolve` and `SetDedupeState` calls make it durable, and only then is the marker updated (5.5). A crash between claim and outcome becomes `unknown` through the existing `Recover`, stays in the journal and appears in `receive_message`, though no pointer announces it (gap G4).

Collection: `receive_message` from a non-nested caller runs `Receiver.Collect(after, limit)`. It returns the same entries as `Mailbox`, plus `mailboxed` ones up to `limit` and a 128 KiB total, always at least one. For each `mailboxed` entry it durably sets dedupe and outcome to `accepted`, re-stamps an inform route, notifies A2A and refreshes the marker, then returns. Order is journal first, so a crash in that window loses the result rather than duplicating it. This matches ADR 0007's no-resend rule and is named as gap G3.

Sender's task: a `mailboxed` inform stays `working` and completes when collected, so `completed` keeps its meaning of harness-side evidence. Ask stays `working` until `reply`, as now. `send_message` returns `mailboxed` with a `hint` (`Result.Hint`, `internal/mcpserver/backend.go:32`) saying the recipient reads it at its next model call and may be idle. A third-party `SendMessage` in default-wait mode blocks until collection or disconnect, which is §5's rule as written. An ask whose deadline passes while mailboxed is still collected, and `reply` returns `ask_closed` (ADR 0010). `CancelTask` on a mailboxed message returns `TaskNotCancelable`, since the claim is already taken. Back-pressure is the existing pending cap (256 unresolved tasks); native frames stay unbounded per WA2.

Route lifetime: ADR 0010's one-hour inform expiry should start when the model sees the message. `Collect` journals a fresh open record with the current time, and re-adds the route if the sweep closed it.

### 5.4 Core changes, separate from the adapter

| # | File | Change |
|---|---|---|
| 1 | `internal/adapter/adapter.go` | Add `Mailboxed`, `Antigravity`; `Injectable` accepts `Mailboxed` |
| 2 | `internal/journal/journal.go` | `StateMailboxed`; `unresolved` keeps it for dedupe and outcome records |
| 3 | `internal/receiver/mailbox.go` | `MailboxStates` and `Admitted` include it; add `Collect`, `MailboxedCount` |
| 4 | `internal/receiver/receiver.go` | `Config.OnCollected`, `Config.OnMailChanged`; call the latter after durable writes and after `Recover`; line 500 treats `Mailboxed` as success |
| 5 | `internal/a2a/mapping.go`, `internal/wire/wire.go` | `outcome` names `Mailboxed` as working; wire `OnCollected` to `Server.Resolve(id, Accepted, "")`; `:331` no reason for it |
| 6 | `internal/shim/send.go` | Lines 529 and 548 count `Mailboxed` as the forward's evidence; `Mailbox` calls `Collect`, or `Mailbox` alone when nested |
| 7 | `internal/harness/harness.go` | `Session.Nested`; optional `MailSignal`, `ToolAnnotator` |
| 8 | `internal/mcpserver/` | `notice` on results; tool `_meta` from `ToolAnnotator`; `instructions`; tool description; list row status |

New optional interfaces: `MailSignal { MailChanged(s Session, pending int) error }`, called after the journal records a change, and `ToolAnnotator { ToolMeta(tool string) map[string]any }`, which adds harness-defined `_meta` to `tools/list` entries.

### 5.5 Hook program: `crosstalk hook antigravity <event>` (adapter)

Stateless, one process per firing, no daemon, never opens the journal. It reads stdin (64 KiB cap), derives `agent_id` from `conversationId` with the existing hash, and reads `~/.local/state/crosstalk/antigravity/mail/<agent_id>.json`, written atomically by the shim as `{"n":3,"pid":…,"started_at":"…"}`. It emits a pointer only if `n` is an integer from 1 to 9999 and that pid is alive with that start time, so a crashed shim cannot cause a pointer-driven retry loop. A subagent's own id has no marker, so it gets no pointer. The shim removes the marker when the agent exits or retires, and rewrites it on registration.

| Event | Output with mail | Otherwise |
|---|---|---|
| `pre-invocation` | `{"injectSteps":[{"ephemeralMessage":"<pointer>"}]}` | `{}` |
| `stop` | `{"decision":"continue","reason":"<pointer>"}` when `executionNum` is 0, `error` is empty, `terminationReason` is not `ERROR`, `fullyIdle` is true | `{}` |

**Fail open.** The spike showed a malformed step errors the whole turn, so the only outputs are these literals, pinned by golden tests. Any panic, parse error or missing file gives `{}` and exit 0. An internal 150 ms timer prints `{}` and exits; `hooks.json` sets `timeout: 2`, the smallest safe whole-second value. What agy does on a missing binary, non-zero exit or timeout is unmeasured (spike S3).

**Stop guard, tighter than the steer.** `executionNum` 0 only gives at most one re-entry per turn, so mail costs at most one extra model call per turn and two agy sessions cannot keep each other's turns alive. The transcript shows the `reason` as a `SYSTEM_MESSAGE` block, which is another reason to keep it fixed. Whether `executionNum` resets on the next prompt in the same conversation is unproven; if it does not, the guard fails safe to one re-entry per conversation.

The hook also touches `antigravity/seen/<agent_id>`, which proves hooks run and gives "last active" for `list_agents` and `--check`. That is the only write.

### 5.6 Sessions, `/clear` and names

v1 follows the rule: without proof, do not retire. After `/clear`, the old conversation's record stays live until agy exits, holding its name. The new conversation registers at its first tool call and gets the fallback name. **Top known gap (G1): a sender using the old name gets `mailboxed` for a conversation nobody is in.** `list_agents` shows both rows with last-active times, and the `hint` says the recipient may be idle, but this is still silent stranding.

**Disagreement with the steer: a current-conversation source can exist.** The shim sees every call's root id, so each agy process has a "latest root" the shim can record in `antigravity/proc/<agy pid>.current`. `Superseded(s)` then reads that file, as Claude's reads its session file. Subagents share the root, so they do not trigger it. This is proof only if an agy process runs one root conversation at a time. Spike S2 tests that (`/fork`, `/resume`, a scheduled wake-up, a background subagent). If S2 passes, commit 11 implements `harness.Supersession` and design §3's readers, retirement and name rules apply unchanged: the old name stops resolving as soon as the new root is recorded, the retire sweep removes the old listeners and marker within 2 s, and `CROSSTALK_NAME` moves to the new agent through the existing per-call name retry. agy has no native row, so unlike Claude the replacement is not listed before its first call. If it fails, or the App shares a process, G1 stays and is reported to the operator. Even with S2, the name moves at the new conversation's first tool call, not at `/clear`. Unread mail stays with the old conversation (§5's rule against routing records into another conversation) and returns on `/resume`.

### 5.7 Discovery and first contact

v1: descriptors only; no `Discover` rows for unbound conversations. A Claude cannot reach an agy conversation until it has called a CrossTalk tool once. This is acceptable for one person who can say "check CrossTalk", and unacceptable if agy is expected to be reachable from cold, so the operator should choose (Q2).

Option offered: `--announce` at install. When `invocationNum` is 0 and no descriptor exists for this `agent_id`, the hook injects a fixed line asking the model to call `list_agents` once. It uses the measured event, needs about 20 lines and costs one model call per conversation. The steer's SessionStart idea has the same cost on an event the spike did not exercise.

Rejected for v1: having the shim register from a hook signal with no tool call. It needs a hook-to-shim path, proof of which agy process fired the hook (unmeasured), and agent creation with no `RequestContext`. Revisit if `--announce` proves too costly.

### 5.8 Install

`crosstalk install antigravity [--name N] [--announce] [--no-allow]`, `uninstall antigravity`, `install antigravity --check`. Documented manual steps are not enough for a non-developer editing three JSON files. Claude and Codex keep their install lines. All code lives in the adapter package; `cmd/crosstalk` dispatches, as it is the sole harness importer.

| File | Key owned by CrossTalk | Value |
|---|---|---|
| `~/.gemini/config/mcp_config.json` | `mcpServers.crosstalk` | absolute binary path, `args:["mcp"]`, optional `env.CROSSTALK_NAME` |
| `~/.gemini/config/hooks.json` | top-level `crosstalk` | `PreInvocation` and `Stop`, absolute path, `timeout:2` |
| `~/.gemini/antigravity-cli/settings.json` | one string in `permissions.allow` | `mcp(crosstalk/*)` |

Rules: an empty file is `{}` (the operator's `mcp_config.json` is 0 bytes today); unparseable JSON aborts with no writes to any file; unknown keys are preserved; temp file plus rename, keeping mode; a `.crosstalk.bak` copy once per file; idempotent; uninstall removes only those keys. It refuses to run while an `agy` process is live, since agy rewrites `settings.json` itself. The allow rule matches Codex's approve-mode line and means a model can message peers without a prompt; `--no-allow` opts out at the price of a prompt on each tool call. `--check` prints each entry, `agy --version`, tested-or-not, and whether any `seen` marker exists.

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
| 1 | Add `mailboxed`; retention keeps it | Fake adapter returns it: state stays `mailboxed` not `unknown`; route open; record survives a 25 h replay |
| 2 | `Receiver.Collect` | Second collect returns nothing; `Mailbox` does not consume; Claude/Codex entries never flip; stale inform route is usable after collect at +2 h |
| 3 | A2A and reply evidence | Inform task `working` then `completed` on collect; ask stays `working`; reply to a mailboxed asker is `delivered` with no retry after restart; `reconcile` after restart |
| 4 | `MailSignal` | Marker equals journal count after accept, collect, `Recover`; marker write ordered after journal; failed marker write leaves state durable |
| 5 | `receive_message`: `notice`, budget, `Nested`, tool `_meta`, `instructions` | Nested call collects nothing; 300 KiB backlog returns under budget and the rest stays; Claude `tools/list` has no agy `_meta` |
| 6 | Adapter package, one line in `cmd` | Identify table (root, conversation only, App path, none); pooled conversations never cross-bind; `arch_test` still passes with no core import |
| 7 | `crosstalk hook` | Golden outputs; marker with dead pid or `"n":"x"` or a body field gives `{}`; hostile stdin gives `{}`; 150 ms bound with a stalled read; Stop guard table |
| 8 | Install, uninstall, `--check` | Fixture: existing hooks and servers survive; 0-byte file; bad JSON writes nothing; install twice is byte-identical; uninstall restores |
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
| 4 | Arrival after last model call | One Stop re-entry; no second | 1 |
| 5 | Claude asks agy; agy `reply`; asker receives with correct `in_reply_to` | Task `completed` with reply text | 1 |
| 6 | agy asks Codex; reply lands mailboxed; collected at next prompt | Round trip complete | 2 |
| 7 | agy → agy | Both ends | 2 |
| 8 | Restart with mail waiting: quit, send fails at sender; `agy --conversation <id>` | Earlier unread mail collected; open ask still answerable | 1 |
| 9 | `/clear` with mail waiting | New conversation has no old mail; behaviour matches 5.6 (gap or commit 11); `/resume` recovers mail | 2 |
| 10 | Remove the binary mid-session | Turn still completes, or the behaviour is recorded and blocks release | 1 |

Quota: about 12 prompts on flash-low with one-sentence prompts; check `/usage` before and after; stop and report if a scenario needs more than twice its budget; repeat only failed rows. Everything testable with `fakeagy` is tested there first. `/schedule` is tested once at most, and only if the operator wants it verified, because each firing costs.

## 8. Rollout

Operator's Mac first, through L1–L10, with the terms question decided. Then the father's Mac: install the same binary, run `--check`, one live inform and one ask with him present, and walk him through section 2 and the terms risk. Rollback is `crosstalk uninstall antigravity`; config returns to prior content and journals remain. Pure-additive for Claude and Codex: no behaviour changes unless a harness returns `mailboxed`.

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
| 8 | Credit spend the father did not expect | One Stop re-entry per turn at most; `/schedule` opt-in; `--announce` opt-in; stated in section 2 |
| 9 | Shared config reaches App and IDE | Stay `unbound`; hooks exit at once because no marker exists. Confirm in S5 |

Known gaps shipped: G1 stranded name after `/clear` unless commit 11 lands. G2 no reach before first tool call unless `--announce`. G3 collected-then-crashed result is lost. G4 a crash-window `unknown` message has no pointer; it appears on any `receive_message`.

## 10. Existing text that must change (commit 10; not edited yet)

| Document | Change |
|---|---|
| ADR 0003 | agy has no registry: not every running session is listed |
| ADR 0006 | Identity list and install line gain agy; note that agy runs `crosstalk hook` and the shim runs one `--version`; neither wraps or spawns a session |
| ADR 0008 | Mailbox capability is now optional `MailSignal`; install and hook entry are additional `cmd` lines; "one package plus one line" becomes three lines for agy |
| ADR 0010 | Inform route expiry for a mailboxed message starts at collection |
| ADR 0007 | No change; ADR 0012 cites it. Its rejected "receiver mailbox" was scoped to Codex |
| `docs/design.md` | Terms; §2; §3 (identity, name, descriptor `harness`); §5 (retention list, task mapping); §6 new adapter; §8 (`receive_message` no longer read-only for `mailboxed`; states list; `accepted` definition); §3 Superseded sessions and §9 Exit (`/clear` takes over the name only where a harness proves supersession); §9 install and "SessionStart hooks are v1.1"; §10 out-list wording on hooks; §11 tests; §12 ADR 0008 note and next harnesses |
| `CLAUDE.md` | Architecture, adapters, trust paragraph; back-link ADR 0012; install command |

## 11. Open questions

For the operator:

- **Q1. Where are the father's peers?** Cross-machine is out of v1. If he has no Claude or Codex on his own Mac, only agy-to-agy works for him. Decide before building.
- **Q2.** Is the first-tool-call rule acceptable, or should `--announce` be on, at one model call per conversation?
- **Q3.** Terms risk (risk 1): proceed, tell the father, and ask Google or not?
- **Q4.** Give up on the `/clear` name if S2 fails, or block release on it?

Further small spikes, each before the commit that depends on it. Total about 7 prompts on flash-low.

| S | Question | Method | Prompts |
|---|---|---|---|
| S1 | Does the CLI read global `hooks.json` and global MCP config? | Put a logging hook in the global file, open agy, `/hooks` and check `agy.log` ("loaded N named hooks") | 0, plus 1 to see it fire |
| S2 | One root per process? | `/fork`, `/resume`, `/schedule`, and a second `agy` (a second process): compare pid and root id on tool calls | 2 |
| S3 | Hook failure modes | PreInvocation with missing binary, exit 1, sleep past timeout; log hook's parent pid | 2 |
| S4 | Tool layer and `send_message` clash | Section 5.9 ladder | 2 |
| S5 | Subagent | Root id vs conversation id on a subagent's tool call; whether hooks fire and with which id; App config check, no prompt | 1 |
| S6 | Mid-turn PreInvocation | Marker written during `sleep 60`; consumed at next model call; `executionNum` on next prompt | 1 |

## 12. Size

About 5 working days of implementation and live testing: core 1.5 (commits 1–5), adapter and hook 1 (6–7), install 0.75 (8), gate and docs 0.5 (9–10), spikes and live run 1, conditional commits 11–12 up to 0.5. Add a day if S1 forces the plugin route.

## Review disposition
