# Plan: a queue wake for `codex --no-daemon` sessions (#112)

Status: DESIGN ONLY. Not approved for build; the operator has approved no implementation. Every decision below is conditional on the feasibility probes (section 5, step 0) and on the operator questions (section 7). Brownfield-gate change plan, revised after the council chair's critique and a one-shot Codex adversarial review (section 11). Decision record: [ADR 0028](../decisions/0028-codex-no-daemon-sessions-get-an-experimental-opt-in-queue-wake-the-journal-stays-the-mailbox.md) (Provisional).

Terms used below. `--no-daemon` session: a Codex TUI started with `codex --no-daemon`, which runs a private app-server embedded in its own process with no socket CrossTalk can dial. Queue: Codex's local SQLite store `~/.codex/queue_1.sqlite`, table `queued_items`, which holds the user's queued prompts per thread and which a watcher in the Codex process dispatches into the thread. Wake row: one row CrossTalk would insert into that table, carrying a fixed sentence and no peer text. Wake: the turn that row starts. Wake generation: CrossTalk's own journal record of one wake row for one thread, with a rising number per thread (section 3.4). Claim: what the SessionStart hook hands to the owning shim, saying "this Codex process serves this session". Owner proof: the check that the claim's process is the shim's own parent and holds the thread's writer lock. Journal: the per-agent append-only store under `~/.local/state/crosstalk/journal/`. `mailboxed` / `collecting`: the journal states of ADR 0012 (durable and pullable, not yet handed to the model / handed out by `receive_message`, receipt not yet evidenced). Evidence tags: measured (seen on a live host), source (read from CrossTalk source, or from Codex source at the `rust-v0.167.0` tag, or binary strings; not run), assumed.

## 1. Goal and ruling

Goal: a `codex --no-daemon` session can receive CrossTalk messages, not only send and pull them (#112). Today it binds with an empty endpoint (`internal/harness/codex/codex.go` `Register`), `Inject` returns `Unreachable` wrapping `ErrNoEndpoint` (`inject.go`), the receiver chain turns an ask into `rejected/no_endpoint` (`internal/receiver/receiver.go`, the `ErrNoEndpoint` branch after `Inject`), and `docs/setup.md` (the `--no-daemon` paragraph) tells the user to run a dedicated app-server instead.

Proposed: an experimental, opt-in, fail-closed route in the Codex adapter only. CrossTalk keeps the authenticated message in its journal, as it already does, records it pullable (`mailboxed`), and only then inserts one fixed-text wake row into the session's thread in Codex's queue. The row tells the model to call `receive_message`, which returns the message as tool output with the existing notice.

What the route does not promise: exactly-once wakes, one wake per N messages, or delivery. It promises at most one open wake per thread, that every message is pullable before any wake for it exists, and that the sender never sees more than the evidence supports.

Registration goal, separate and must-pass: a session is registered and receivable before its first message is sent to it, not before the operator's first prompt. Once SessionStart has fired, registration does not depend on the session's own first CrossTalk call. A tab nobody has prompted fires no hook (measured), so literal tab-open discovery needs a separate probe of parent-owned writer locks (section 5, step 0); it is not promised by this plan.

Ruling to date: a council of five advisors over two rounds (one Codex, four Claude) and the coordinator's concerns. The chair's synthesis, accepted by the coordinator: a bounded feasibility spike first; then, at most, an experimental opt-in capability, off by default and never described as production-ready. Every advisor rated the Codex executor's position strongest: keep daemon delivery as the supported route, keep the envelope in the journal, put only a constant wake in the queue, and reject the route if peer text becomes operator input, a wake replays unintended work, or pending mail disappears. The operator asked for this experiment; the consent choices are in section 7.

Non-goals: a general wake primitive for other harnesses; a broad SQLite abstraction; any change to the supported daemon or dedicated-app-server routes; an upstream Codex issue or feature request (only with the operator).

## 2. Existing state (what the route must fit)

- **One receiver chain.** authenticate → dedupe (`AcceptRecord`) → hop-chain and policy (admission) → dispatch claim (`ClaimFor`, journal state `dispatching`) → one `r.ad.Inject` → `ResolveFor` records the returned state (source, `receiver.go`). The outcome is journaled only after `Inject` returns. Harness behaviour lives in the adapter (AGENTS.md canonical pipelines, ADR 0008).
- **Outcome records are last-writer-wins.** `ResolveFor` appends an outcome record and the latest one is the state (source, `journal.go`); nothing stops a late outcome from overwriting a later mailbox state. The mailbox states need a monotonic rule (section 3.2).
- **Codex adapter.** `Register` binds a host-less thread with an empty endpoint; `hosts` counts a host only from `thread/loaded/list` on a dialable endpoint (design §3). `Inject` returns `ErrNoEndpoint` when no endpoint is found after rebind. A queue-deliverable target must stop returning `ErrNoEndpoint`, or its asks stay refused.
- **Per-request identity and pooling.** Identity is bound per `tools/call` from `params._meta.threadId`; one process may serve several threads, and calls from several threads and repeated calls from one thread run concurrently on one connection (ADR 0006; `TestPooledThreadsNeverCrossBind` in `internal/mcpserver/server_test.go`). A `--no-daemon` process can also host more than one thread (subagents), so "one shim, one thread" is not assumed.
- **States.** `adapter.State` has `accepted`, `queued`, `rejected`, `unreachable`, `unknown` (and `bounced` for mailbox entries). ADR 0012 defines `mailboxed` and `collecting`, but ADR 0012 is planned and unbuilt: no `mailboxed` state and no `MailSignal` interface exist in code. This route would be the first user of that contract, so its shared parts are built here, harness-neutral.
- **Pull filter.** `receive_message` never returns `accepted` or `queued` entries (`internal/mcpserver/server.go`, design §8). Its `after` argument is a message-id cursor. A wake that pointed at a `queued` entry would point at a message the pull hides; this is why the route returns `mailboxed` and never `queued`.
- **ADR 0007.** Peer text never enters operator input; `queued` means handed to model history; "No subprocess queue is used". ADR 0004: peer text never enters instructions. A user-role wake, even with fixed text, is an exception that needs ADR 0007 amended, not a silent one.
- **Unverified senders and `next_turn`.** Both use the deferred no-wake path today; unverified senders never start a turn (design §6, §7).
- **Replay.** A `dispatching` claim is reconciled only through `thread/turns/list` on an endpoint (design §5). With no endpoint, an unresolved claim can only become `unknown`.
- **Pre-bind store.** Informs injected before a session binds are kept and replayed at bind (ADR 0026).
- **Version gate.** `internal/harness/codex/version.go` reads the app-server `initialize`; `TestedMinors` is 0.166 while the installed CLI is 0.167.0 (measured). The queue route has no `initialize` to read.
- **Release.** The release binary is signed and static (ADR 0015, 0017); `go.mod` has no SQLite driver. Any driver must be cgo-free.
- **Docs.** `docs/setup.md` (the `--no-daemon` paragraph and troubleshooting rows) and the FAQ known issue say such sessions cannot receive.

Facts the design rests on:

- **Startup order (measured).** The CrossTalk shim starts about 1 s after the Codex process (three of three). SessionStart fires at the first turn, not at launch: five minutes after launch in one tab, two seconds in a tab whose prompt was passed on the command line. An unprompted tab fires no hook.
- **Process shape (measured).** Each `--no-daemon` process has its own shim child, whose parent is the `codex --no-daemon` process. Daemon parents carry two to four pooled shims. The shim's environment has no `CODEX_THREAD_ID`; Codex sets it for hooks and shell tools (source). The owning process holds `~/.codex/thread-writer-locks/<thread>.lock` and the rollout file open (measured with lsof).
- **Hook payload (source, embedded schema).** SessionStart input carries `session_id`, `cwd`, `model`, `permission_mode`, `hook_event_name`, `source` (startup, resume, clear, compact, fork) and `transcript_path` (may be null, seen null in practice). It fires again in a live session (measured twice in one tab). `session_id` equal to the thread id is assumed, not measured.
- **Queue watcher at 0.167.0 (source, `codex-rs/ext/queue/src/service.rs` at tag `rust-v0.167.0`, read by the council chair, not measured).** The watcher runs every 10 s (lines 89-96). It skips threads that are Running, Interrupted, Shutdown or NotFound (220-223), and excludes Interrupted again at 477. A row that is invalid or not a user input is deleted and the loop continues (428-435), so a bad row does not block the queue head at this version. A row is removed when Core accepts the turn (440-447), before any proof that the model fetched anything. The queue holds at most 100 items (`MAX_QUEUE_ITEMS`, `codex-rs/state/src/lib.rs`, same tag).
- **Queue schema (source, research).** Introduced 2026-08-04 with six columns and a unique `(thread_id, queue_order)` index; dispatch serializes `TurnInput` as JSON (08-06); `queued_thread_revisions`, a backfill and three triggers added for cross-process change detection (08-17), which is what makes an external insert visible; immediate transactions and retries for writer contention (10-15); optional client metadata in stored JSON (11-06, after 0.167.0, backward compatible). Two queue migrations exist at 0.167.0. Observed stability over 98 days, not an external-write contract.
- **Live trials (0.167.0), recorded per trial.** These were manual inserts, not product code.
  - Trial A, idle thread (measured): one fixed wake row inserted into an idle thread started a turn.
  - Trial B, idle advisor thread (measured): one fixed wake row started a turn in which the model called `receive_message` and then answered with `reply`. This is the only trial that shows the full nudge → pull → reply path. Removal of the row after dispatch was observed in one of trials A and B.
  - Trial C, busy thread (measured): two fixed wake rows were inserted for a thread that was mid-turn. One row was still present 15 s later; at the time of the chair's critique both rows were still present in the queue. The messages were pulled by hand in that session, not by a wake. This trial shows only that a row for a busy thread persists and that the mailbox stays readable by a manual pull; it does not show queue dispatch. Why the rows had not dispatched (the thread stayed Running, or that thread was not owned by a queue watcher) was not established. The two rows are still in that host's Codex queue; whether to remove them is the operator's (section 7, (d)).
- **Not verified.** Dispatch after a busy turn ends; "locally loaded" scope; whether a running daemon also dispatches rows for a thread (no evidence; the watcher uses its own loaded thread manager and the writer lock gates ownership, so a double wake is a hypothesis); whether `session_id` equals `_meta.threadId`; resume, compaction and crash behaviour. The supported dedicated app-server plus `--remote` route has never been run live either.

## 3. Change (conditional: if the probes pass and the operator approves)

### 3.1 Route selection (Codex adapter, per delivery)

Exactly one row applies to each delivery, checked in order:

| # | Condition | Route | State returned | Wake |
|---|---|---|---|---|
| 1 | `hosts()` finds exactly one host for the thread | app-server (unchanged) | as today | n/a |
| 2 | The app-server route already returned `unknown` for this message | none (no fallback, no retry) | `unknown` | no |
| 3 | No host, and queue capability absent: setting off, no owner-proven claim (3.3), or gate (3.5) fails | none | `unreachable` with a hint; an ask is `rejected/no_endpoint` | no |
| 4 | No host, queue capability present, and the delivery is `next_turn` or from an unverified sender | mailbox only | `mailboxed` | no |
| 5 | No host, queue capability present, `immediate` from a verified sender | queue wake | `mailboxed` | at most one open generation per thread (3.4) |

Queue capability present means all of: the setting is on; an owner-proven claim for this thread holds at write time; the gate passes. Rows 4 and 5 both require it, so a `next_turn` or unverified message to a session with no capability is row 3 (`unreachable`), never silently mailboxed. Asks follow the same table: in rows 4 and 5 an ask is accepted in state `mailboxed`, its route open and deadline armed as for any ask; in row 3 it stays `rejected/no_endpoint`. Asks are accepted only after the step 0 probes and the step 7 live acceptance pass; until then the capability is never reported present for an ask.

### 3.2 States, ordering and the pull (shared, harness-neutral; ADR 0012's contract)

- **Pullable before any wake.** The queue route's `Inject` writes nothing external and returns `mailboxed`, as ADR 0012 already specifies. The receiver records `mailboxed` (`ResolveFor` and the dedupe state) and only after that write commits calls the adapter's optional `MailSignal` (the ADR 0012 extension point) for the thread. The Codex adapter's `MailSignal` is the only code that writes a wake row. So every entry a wake can lead to is already pull-visible when the row exists, and a crash before the signal leaves a pullable entry with no wake. The chain order authenticate → dedupe → hop-chain → policy → `Inject` → resolve is unchanged; the signal is a new step after resolve, harness-neutral and a no-op for adapters without `MailSignal`.
- **Monotonic mailbox states.** For an entry, `mailboxed` → `collecting` → `accepted` or `unknown` only; no transition goes back, and an outcome record that would move an entry backwards (a late dispatch outcome, a replayed resolve) is ignored. A test pins that a late `ResolveFor` cannot overwrite `collecting` or `accepted`.
- **Pull.** `receive_message` returns `mailboxed` entries (the filter keeps excluding `accepted` and `queued`). A top-level `receive_message` from the bound thread takes the per-agent mail lock, marks the returned entries `collecting` with a collection token (journal-internal, one per pull), restarts inform route clocks, and returns the bodies with the fixed no-operator-authority notice, a `next` cursor and the token.
- **Settlement needs causal evidence of receipt.** A later call is evidence only if it could not have been made without the pull's result. An entry becomes `accepted` only on one of: a `reply` naming that entry's message id; a later `receive_message` whose `after` cursor is at or past the entry (cursors come only from pull results; the wake carries none); or a later call that echoes the pull's collection token in an optional `ack` argument. Any other call, including a concurrent `list_agents` or a second pull on the same thread with no cursor, is not evidence, because Codex can issue tool calls concurrently and a call issued alongside the pull says nothing about what the model read. The token is receipt evidence only, never identity: identity still comes from `_meta.threadId` (ADR 0004, 0006). A call from a different thread, including a subagent of the same process, is never evidence for this thread's entries (ADR 0012 item 6).
- **No evidence → `unknown`.** A `collecting` entry with no causal evidence within a fixed bound, at exit, or on replay becomes `unknown`: readable, unresolved, never re-woken. CrossTalk does not force a tool call only to acknowledge; the skill and the tool description may say that `ack` is optional. An ask stays open until `reply`; `reply` to an entry still `mailboxed` returns `not_collected`.
- **Drain.** The result of a `receive_message` that leaves `mailboxed` entries behind (a page limit, or a message that arrived during the call) says so in a fixed field, so one wake can drain several pages within its turn.
- The ADR 0012 extension points (`MailSignal`, the `mailboxed` state, the monotonic rule, the causal settlement rule) are built once and named for no harness. ADR 0012 item 3 ("next top-level bound call") is narrowed in place to the causal rule when this is built.

### 3.3 Registration and ownership (SessionStart, a separate must-pass goal)

- `crosstalk hook codex session-start` is stateless and never opens the journal. It hands its claim to the owning shim through an authenticated exchange and also leaves it durably (a 0600 file under `~/.local/state/crosstalk/codex-claims/`), so the handoff survives a shim that is not listening yet. It prints `{}` and exits 0 on any failure, under a short internal time bound. The claim holds `session_id`, `source`, `cwd`, `transcript_path`, the nearest ancestor `codex` pid with its start time, `CODEX_THREAD_ID` if set, and a timestamp; the latest claim per (pid, session_id) wins. It is idempotent across resume, clear and compaction refires.
- Owner and thread lineage proof: the claim's codex pid and start time equal the shim's own parent, and that parent holds `thread-writer-locks/<session_id>.lock` open (a cgo-free process file-descriptor scan). A pooled shim whose parent is `codex app-server` never matches and keeps the app-server route.
- With the proof, the shim registers the agent for that thread from the claim, without waiting for the session's own first CrossTalk call. Making that first `tools/call` a precondition would repeat today's circular dependency.
- **Claims are per (harness, thread), not per process.** One process may hold several claims, one per thread, and each is its own agent record. A `tools/call` carrying another valid `_meta.threadId` binds per request as today (ADR 0006) and neither revokes nor touches another thread's claim. A claim-based binding and its queue route are revoked only for that claim's thread, when: the owner proof fails on recheck (the parent no longer holds that thread's writer lock, or the parent pid and start time changed), or a claim for the same `session_id` arrives from a different process. A per-request binding for the claimed thread (its own `_meta.threadId` on this shim's connection) confirms the claim, never replaces the proof.
- Without the proof, the row shows discovered and unbound only. Such a claim never unlocks a queue write, an ask, a name pin, a `claim` or `command` target, or a rebind of an existing agent record; informs to it wait in the journal or the ADR 0026 pre-bind store, with a sender hint. A claim for a `session_id` already bound elsewhere is ignored.
- Whether the owner proof is sufficient is an engineering obligation (step 1 tests and the step 7 live run), not an operator question.
- Acceptance check for this goal, separate from the queue route: in a prompted `--no-daemon` tab, the claim arrives, the proof holds, and a message sent before the session's first CrossTalk call is held as `mailboxed` for the right thread with no duplicate agent; two valid threads served by one process and one shim each get their own record, and calls for both, concurrent, never cross-bind or revoke each other; in a pooled daemon session the proof fails; a forged claim from another process fails; a revoked claim stops queue writes for its thread only. An unprompted tab is out of this check and depends on the separate lock-discovery probe.

### 3.4 The wake row and its lifecycle

- **Payload.** One plain-text user input with a fixed sentence naming `receive_message` on server `crosstalk`, no sender field, no count, no message id, no peer text. A CrossTalk wake id (thread plus generation number) rides in the row's own client id field so CrossTalk can recognize its own rows. The fetched envelope, not the wake, carries the message id, so `receive_message` stays idempotent by message id.
- **Wake generations (journal, per thread).** Each wake is a persisted generation `g` with one of these states, written before the action it names:
  - `pending`: recorded before the queue transaction starts.
  - `queued`: the queue transaction committed with CrossTalk's row for `g`.
  - `admitted`: CrossTalk's row for `g` was observed gone (Codex took the turn, or deleted the row as invalid) and no pull has followed yet. Unconfirmed.
  - `collected`: a top-level `receive_message` from that thread ran after `g` was `queued`. Closed.
  - `unknown`: closed without evidence (see the ambiguity policy). Closed.
  - `failed`: the transaction did not commit (busy timeout, cap, gate failure at write time). Closed; the entry stays `mailboxed` and the sender's state does not change.
- **Open generation guard.** A generation is open while `pending`, `queued` or `admitted`. `MailSignal` writes a new row only when the thread has no open generation; otherwise it does nothing, and the open wake's turn will see the new entry because it was made pullable first (3.2). A message that arrives in the gap between Codex taking the row and the model's pull therefore enqueues nothing.
- **Write.** One `BEGIN IMMEDIATE` transaction that revalidates the gate (3.5), checks the table for any CrossTalk row for the thread, and if none, inserts at `MAX(queue_order)+1` for that thread, with a bounded busy retry; a busy timeout is `failed`, never a blind retry later. A thread at the 100-item cap gets no row (`failed`).
- **Ambiguity policy.** Ambiguity resolves toward fewer paid turns and an honest state, never toward a guess:
  - `admitted` with no pull within a fixed admission bound becomes `unknown`. Its entries stay `mailboxed` and pullable; they do not by themselves cause another wake. The next newly arriving message may open generation `g+1`. So a lost or ignored wake costs at most one more wake per later arrival, never a loop.
  - `pending` found on replay: CrossTalk looks for its row by wake id. Found: `queued`. Not found: the commit never happened, or it happened and Codex already took the row; the two cannot be told apart, so `g` becomes `unknown` and no row is written on replay.
  - `queued` found on replay with the row still present: stays `queued` (it may still fire). With the row gone: `admitted`, then the bound applies.
  - Replay and bind never open a generation by themselves; only a newly arriving message does.
- **Interrupted or paused stays paused.** The 0.167.0 watcher skips interrupted threads (source), and CrossTalk does not try to defeat that; the entry stays `mailboxed` and pullable, and the generation stays `queued`.
- Row removal proves only that Codex took the turn or dropped the row, not that the model fetched anything; it is never delivery evidence.
- Write only while the owner proof holds at write time, and never when `hosts()` finds the thread on any endpoint.

### 3.5 Gate (fail closed, revalidated per write)

- Checks: `queue_1.sqlite` already exists (CrossTalk never creates it); the table layout matches; the queue migration versions and checksums and the three revision triggers match a pinned set; the parent Codex binary's version is on a tested allowlist (0.167 only until the probes run on others).
- **No positive result is cached across writes for the database.** Each write opens the file, checks that its device and inode equal the path's current ones (a replaced or migrated file is a new file), and re-reads the schema, migration rows and triggers inside the same `BEGIN IMMEDIATE` transaction as the insert, so a migration by another Codex process between check and insert is impossible. The connection is closed after the write.
- **Version.** Read once per (parent pid, start time) and cached for that pair only. The running parent's code cannot change, but the file on disk can: the version is taken only while the parent's mapped executable is the same file (device and inode) as the path; if the binary was replaced while the parent runs, the version is unknown and the gate fails. A new Codex process (an upgrade, a resume) is a new pair and is checked afresh.
- Any mismatch: no write, `unreachable` with a hint that names the supported dedicated app-server route for new messages, and every existing entry stays pullable.

### 3.6 Crash windows and transport switch

Each window below is a deterministic failure-injection test in steps 2 and 5.

- Crash after `mailboxed` is recorded and before `MailSignal`: entry pullable, no generation; the next arriving message may open one.
- Crash after `pending` and before the queue commit, or after the commit and before `queued` is recorded: replay applies the ambiguity policy (3.4); no second row.
- Pause after the queue commit, then pull at once: the entry is returned, because it was `mailboxed` before the row existed.
- Crash after the pull marks `collecting` and before the response reaches Codex: the entries become `unknown` at the bound; a concurrent `list_agents` on that thread does not settle them.
- Codex dies with the row unconsumed (kill -9): no SessionEnd runs; the row stays in Codex's database. A later `codex resume` of that thread may dispatch it; the wake is harmless by construction (fixed text, nothing to replay but a pull). CrossTalk never deletes a row in v1 (section 7, (d)).
- Queue to daemon switch: when a resumed thread appears on an app-server, the queue route stops for it; `mailboxed` entries stay pullable and are not re-sent as tool output, so one envelope never enters both paths.

### 3.7 Sender view

- The sender sees `mailboxed` with a hint: delivery is a best-effort wake that fires only when the session is idle (watcher interval 10 s at 0.167.0, not a deadline), not `delivered`. `list_agents` shows the delivery mode for the row (tool output, queue wake, or pull only) so the sender can judge an ask's deadline.
- Fetched peer text stays data under the existing notice and the skill, although the wake that prompted the fetch is a user-role turn.

## 4. Public surface to change

- Design §3: the delivery-mode field on Codex rows; the claim handoff and owner proof in registration; per-thread claims; the discovered-unbound row.
- Design §4/§8: the `mailboxed` state in the state list; `receive_message` returns `mailboxed` entries, the drain field, the `next` cursor and the collection token; the optional `ack` argument; ask acceptance in `mailboxed`; the sender hint text.
- Design §5/§6: the post-resolve `MailSignal` step; the queue route beside tool output and deferred append; the wake generation records.
- ADR 0007 amended in place (the user-role wake exception, "no subprocess queue" narrowed); ADR 0012's state contract marked shared and item 3 narrowed to causal settlement; ADR 0004 cross-reference; ADR 0028 new.
- One setting, off by default (section 7, (a)): one install flag turns it on, writes the SessionStart hook and the setting, and `uninstall` removes only what it added. `install --check` gains a warn-only line for the gate. The flag's name is an implementation choice.
- `docs/setup.md` (`--no-daemon` paragraph, troubleshooting rows) and the FAQ known issue: experimental, opt-in, each wake a paid turn, already-written rows may fire after the setting is off, supported route named first.
- AGENTS.md Architecture: one clause naming the route and ADR 0028.

## 5. Commit sequence (small, test-pinned)

0. **Feasibility probes (raw Codex and hook behaviour; no product code).** A throwaway harness outside the product tree, using hand-written inserts and a manual `receive_message`, drives a `--no-daemon` Codex 0.167 session on a scratch thread and records, for each case, the queue table before and after, the rollout file, the TUI, and what the model did. These probes measure only what Codex and its hooks do; they test no CrossTalk state machine.
   - Idle fresh thread: one row, one dispatch, row removed; time from insert to dispatch recorded.
   - Busy mid-turn: the row waits and dispatches once after the turn ends, never steering mid-turn; consumption time recorded (trial C did not show dispatch at all).
   - Interrupted: the row does not dispatch while interrupted.
   - Resumed (same and other process): a row left from before fires at most once in total.
   - Compaction: a row neither replays nor duplicates; SessionStart refire payloads recorded.
   - kill -9 at consumption: no second dispatch of an already-consumed row on resume.
   - Two rows queued (forced): observe order and duplicate turns.
   - Operator draft in the composer: the draft is not lost, submitted or merged with the wake.
   - Operator's own queued prompt present: order preserved, the operator's item never reordered.
   - Competing ownership and ownership transfer: a daemon and a `--no-daemon` process on the same thread in turn; only the writer-lock holder dispatches, and a transfer neither drops nor doubles the row.
   - Subagent threads: whether a `--no-daemon` process serves more than one thread on one shim connection, and with which `_meta.threadId` values.
   - Hook `session_id` equals `_meta.threadId` on the same thread, across startup, resume and fork.
   - Lock and process facts the owner proof needs: the writer lock is held by the owner only, released at exit, and readable by a cgo-free fd scan; the parent's mapped executable can be compared with the path.
   - Tab-open discovery (separate probe): whether a parent-owned writer lock alone identifies an unprompted tab's thread.
   - Model treats fetched text as data: a manually pulled body that asks for an action is reported, not acted on.
   Reject the route if any case shows peer text becoming operator input, a wake replaying unintended work, or pending mail disappearing. Report the probe results to the operator before step 1.
1. **SessionStart claim handoff and owner proof.** Hook command, authenticated exchange, durable claim, lineage check, per-thread claims, revocation rules; tests for a forged claim, a pooled shim, pid reuse after the owner exits, a lock held by another process, two valid threads in one process and one shim with concurrent calls (no cross-bind, no cross-revoke), refire idempotence, unproven-claim limits, a message held before the first CrossTalk call. Must pass on its own; the queue route does not start without it.
2. **Shared `mailboxed` / `collecting` states.** Journal records, the monotonic rule, the post-resolve `MailSignal` step, pull filter, collection token and cursor, causal settlement, the bound to `unknown`, `not_collected`, replay to `unknown`; harness-neutral tests with `adapter.Fake`, including: a late `ResolveFor` cannot overwrite `collecting` or `accepted`; a concurrent pull and `list_agents` on one thread with the pull response withheld leaves the entries `collecting` and then `unknown`; a `reply`, a covering cursor or an echoed token settles `accepted`; a call from another thread never settles.
3. **Gate.** cgo-free SQLite read of schema, migrations, triggers inside the write transaction; file identity check; version per (pid, start time) with the executable identity check; table test against fixture databases (matching, extra migration, missing trigger, missing file, file replaced between writes, migration applied between writes).
4. **Queue write and wake generations.** One transaction, the open-generation guard, wake id, busy retry bound, the 100-item cap, the generation states and the ambiguity policy; tests against a fixture database for concurrent writers, a message arriving while `admitted`, the admission bound, replay of `pending` and `queued` with and without the row, and no row on any gate failure.
5. **Route selection in `Inject`.** The section 3.1 table, one test per row, including `next_turn` and unverified senders with no capability (`unreachable`, not `mailboxed`); asks accepted `mailboxed` only when the capability is present; the crash-window and transport-switch tests of 3.6.
6. **Sender hint, delivery mode, install flag, `install --check` line, docs, ADR 0007, ADR 0012 and design edits.**
7. **Live acceptance run (gate for asks and for calling the route usable).** On a `--no-daemon` 0.167 session with the built binary: idle wake → pull → reply settles `accepted`; busy wake dispatches after the turn; pull then final answer with no further call ends `unknown`; owner proof holds for the real session and fails for a pooled daemon shim; the setting off behaves exactly as today. Results recorded per trial in the ADR's trial record.

Every implementation commit carries `Provisional-ADR: 0028`.

## 6. Risks

- **Private store, no contract.** A later migration, a validation change or a JSON decoding change can make the row invalid or change dispatch. At 0.167.0 an invalid row is deleted and the loop continues (source), so the row cannot block the operator's queue at that version; a later version is covered only by the per-write gate and the allowlist.
- **User-role authority.** The wake shows as a user turn. Any same-user process could already write this database; CrossTalk adds a fixed sentence, never peer text. The fetched body still arrives as tool output with the notice; the probe checks the model keeps treating it as data.
- **Spend and noise.** Each wake starts a paid model turn and is visible to the operator. The guard bounds open wakes per thread, not messages, and the ambiguity policy can cost one extra wake per later arrival after a lost wake. Opt-in only.
- **No consumption evidence by default.** Codex removes the row when it takes the turn, before any fetch; the model may ignore the wake or stop on an approval prompt. A model that pulls and ends its turn without a reply, a cursor or an `ack` leaves its entries `unknown` at the bound; most informs may end `unknown` in practice. The sender's hint never claims delivery.
- **Lost wake.** A `pending` generation that never committed, or a wake Codex dropped, leaves mail pullable but unannounced until the next arrival or a manual pull.
- **Pending rows outlive the setting.** A row already in Codex's queue may fire after the setting is turned off, after a Codex upgrade turns the gate off, or on resume; CrossTalk writes no new row in those cases but does not remove the old one in v1.
- **Queue cap.** A thread with 100 queued items gets no wake.
- **Identity.** `session_id` equal to the thread id is assumed until the probe; a wrong assumption fails closed (no bind, no write).
- **cgo-free SQLite** must handle WAL and Codex's locking correctly; a driver that does not is a corruption risk to the operator's queue.

## 7. Open questions for the operator (consent)

The operator asked for this experiment, so whether to write Codex's queue at all is not asked here; the probes decide feasibility. Flag naming, the owner proof's sufficiency and the state machine are engineering choices pinned by tests. What is left is consent:

- (a) **Opt-in scope.** Recommended: one setting, off by default, turned on by one install flag, applying to every `--no-daemon` session of that user on that host while on; no per-session or per-sender switches in v1; labelled experimental wherever it shows.
- (b) **Paid user-role wake turns.** Is a fixed-text user-role wake that starts a paid model turn accepted as a named exception to ADR 0007 and ADR 0004? Recommended: yes, narrowly: fixed text only, no peer text, at most one open wake per thread, only while the setting is on, and only for `immediate` messages from verified senders.
- (c) **Pending wakes after disable, upgrade or resume.** A row CrossTalk already wrote can fire after the setting is turned off, after a Codex upgrade fails the gate, or when a thread is resumed. Recommended: accept that such a row may fire once (it only asks for a pull), write no new row in any of those cases, and say so in the docs; the alternative needs (d).
- (d) **Deleting CrossTalk's own stale wake rows.** Recommended: defer from v1; v1 deletes no row in Codex's database, and the question is revisited with probe data. The two rows left by trial C on the test host are the operator's to keep or remove.

## 8. Decided

Nothing is decided by the operator. Recorded from the council, the coordinator and the reviews, binding on any build, all conditional on the probes: spike first; experimental and off by default; journal authoritative; entries pullable before any wake (post-resolve `MailSignal`); monotonic mailbox states; ADR 0012 states reused, no `accepted` before handoff; settlement only on causal evidence (reply, covering cursor or echoed token), else `unknown` at a bound, no forced tool calls; persisted wake generations with one open per thread and ambiguity resolved toward fewer wakes; no promise of exactly-once or one wake per N messages; queue writes only in the Codex adapter; fail-closed gate revalidated in the write transaction; mutually exclusive route table; `next_turn` and unverified senders never wake; no fallback to user input and no automatic retry after an ambiguous daemon send; asks accepted `mailboxed` only after the probes and the live acceptance run; registration from an owner-proven claim per (harness, thread), not from the first `tools/call`; honest sender state; no message rate limit, bounded redundant wakes; never delete unknown or user rows; no broad SQLite abstraction; cgo-free driver; no upstream Codex issue without the operator.

## 9. Rollout

Behind one setting, off by default (section 7, (a)), labelled experimental in the install output, the setup page and the `list_agents` delivery mode. A session without the setting behaves exactly as today (`unreachable`, `no_endpoint`), pinned by a test. Turning the setting off removes the hook and stops new queue writes; `mailboxed` entries stay pullable, and a row already written may still fire once (section 7, (c)). A Codex upgrade outside the allowlist turns the route off by the gate, with the hint, until the probes are re-run for that version.

## 10. Codex review

Run once, as a one-shot adversarial review of the branch against `main` (the companion runtime, not the standing reviewer session); verdict needs-attention with two high findings. Dispositions are in section 11. The review ran no tests and did not check live Codex behaviour or the cited upstream source.

## 11. Review dispositions

Council chair critique:

- Chair 1 (pooled-thread revocation): fixed. Claims are per (harness, thread); another thread's call never revokes; revocation conditions narrowed to the claim's own thread; two-thread-one-process test added (3.3, step 1, ADR 0028 item 8). Verified: ADR 0006 and `TestPooledThreadsNeverCrossBind` require per-request identity on a shared connection.
- Chair 2 (wake/mailbox lifecycle): fixed. Persisted wake generations with `pending`, `queued`, `admitted`, `collected`, `unknown`, `failed`; open-generation guard; explicit ambiguity policy; failure-injection tests; exactly-once and N-to-one promises removed (1, 3.4, 3.6, steps 2 and 4, ADR 0028 item 4).
- Chair 3 (evidence correction): fixed. Trials recorded one by one; trial C shows row persistence and a manual pull only, not dispatch; only trial B shows nudge → pull → reply (2, ADR 0028 context).
- Chair 4 (gate sequence): fixed. Step 0 is raw Codex and hook probes only; owner-proof sufficiency and acknowledgement semantics moved to step 1 and step 2 tests and a new step 7 live acceptance run (5).
- Chair 5 (cached compatibility): fixed. No cached positive gate result for the database; file identity and schema revalidated inside the write transaction; version cached per (pid, start time) with an executable identity check (3.5, step 3, ADR 0028 item 5).
- Chair 6 (route table contradiction): fixed. Mutually exclusive table; `mailboxed` without a wake requires proven capability, absent capability is `unreachable` (3.1, step 5, ADR 0028 item 6). Verified: the old text sent `next_turn` and unverified messages to `mailboxed` while also saying "otherwise unreachable".
- Chair scope note (one off-by-default setting): fixed. Recommended in section 7 (a); the flag's name is left to the build (4).
- Chair scope note (defer stale-row cleanup): fixed. v1 deletes nothing; recommended in section 7 (d).

Codex adversarial review:

- Codex 1 (wake can precede mailbox visibility): fixed. Verified against `receiver.go`: the outcome is journaled only after `Inject` returns, and `ResolveFor` is last-writer-wins. `Inject` now writes nothing external; the row is written by a post-resolve `MailSignal`; mailbox states are monotonic; pause-after-commit and crash-before-final-write tests added (3.2, 3.6, step 2, ADR 0028 item 2).
- Codex 2 (a later call does not prove receipt): fixed. Verified: the MCP server serves concurrent calls, including repeated calls for one thread. Settlement now needs causal evidence (a `reply` naming the entry, a covering `after` cursor, or an echoed collection token); a concurrent pull-and-list test with the response withheld added (3.2, step 2, ADR 0028 item 2). ADR 0012 item 3 is narrowed to match when built.
