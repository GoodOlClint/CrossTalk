# Plan: a queue wake for `codex --no-daemon` sessions (#112)

Status: DESIGN ONLY. Not approved for build; the operator has approved no implementation. Every decision below is conditional on the probe matrix (section 5, step 0) and on the operator questions (section 7). Brownfield-gate change plan. Decision record: [ADR 0028](../decisions/0028-codex-no-daemon-sessions-get-an-experimental-opt-in-queue-wake-the-journal-stays-the-mailbox.md) (Provisional).

Terms used below. `--no-daemon` session: a Codex TUI started with `codex --no-daemon`, which runs a private app-server embedded in its own process with no socket CrossTalk can dial. Queue: Codex's local SQLite store `~/.codex/queue_1.sqlite`, table `queued_items`, which holds the user's queued prompts per thread and which a watcher in the Codex process dispatches into the thread. Wake row: one row CrossTalk would insert into that table, carrying a fixed sentence and no peer text. Wake: the turn that row starts. Claim: what the SessionStart hook hands to the owning shim, saying "this Codex process serves this session". Owner proof: the check that the claim's process is the shim's own parent and holds the thread's writer lock. Journal: the per-agent append-only store under `~/.local/state/crosstalk/journal/`. `mailboxed` / `collecting`: the journal states of ADR 0012 (durable, not yet handed to the model / handed out by `receive_message`, not yet evidenced). Evidence tags: measured (seen on a live host), source (read from CrossTalk source, or from Codex source at the `rust-v0.167.0` tag, or binary strings; not run), assumed.

## 1. Goal and ruling

Goal: a `codex --no-daemon` session can receive CrossTalk messages, not only send and pull them (#112). Today it binds with an empty endpoint (`internal/harness/codex/codex.go` `Register`), `Inject` returns `Unreachable` wrapping `ErrNoEndpoint` (`inject.go`), the receiver chain turns an ask into `rejected/no_endpoint` (`internal/receiver/receiver.go`, the `ErrNoEndpoint` branch after `Inject`), and `docs/setup.md` (the `--no-daemon` paragraph) tells the user to run a dedicated app-server instead.

Proposed: an experimental, opt-in, fail-closed route in the Codex adapter only. CrossTalk keeps the authenticated message in its journal, as it already does, and inserts one fixed-text wake row into the session's thread in Codex's queue. The row tells the model to call `receive_message`, which returns the message as tool output with the existing notice.

Registration goal, separate and must-pass: a session is registered and receivable before its first message is sent to it, not before the operator's first prompt. Once SessionStart has fired, registration does not depend on the session's own first CrossTalk call. A tab nobody has prompted fires no hook (measured), so literal tab-open discovery needs a separate probe of parent-owned writer locks (section 5, step 0); it is not promised by this plan.

Ruling to date: a council of five advisors over two rounds (one Codex, four Claude) and the coordinator's concerns. The chair's synthesis, accepted by the coordinator: a bounded feasibility spike first; then, at most, an experimental opt-in capability, off by default and never described as production-ready. Every advisor rated the Codex executor's position strongest: keep daemon delivery as the supported route, keep the envelope in the journal, put only a constant wake in the queue, and reject the route if peer text becomes operator input, a wake replays unintended work, or pending mail disappears. The operator asked for this experiment; the open choices are in section 7.

Non-goals: a general wake primitive for other harnesses; a broad SQLite abstraction; any change to the supported daemon or dedicated-app-server routes; an upstream Codex issue or feature request (only with the operator).

## 2. Existing state (what the route must fit)

- **One receiver chain.** authenticate → dedupe (`AcceptRecord`) → hop-chain and policy (admission) → dispatch claim (`ClaimFor`) → one `r.ad.Inject`. Harness behaviour lives in the adapter (AGENTS.md canonical pipelines, ADR 0008). The queue write must be an outcome of the Codex adapter's `Inject`; the chain does not change order.
- **Codex adapter.** `Register` binds a host-less thread with an empty endpoint; `hosts` counts a host only from `thread/loaded/list` on a dialable endpoint (design §3). `Inject` returns `ErrNoEndpoint` when no endpoint is found after rebind. A queue-deliverable target must stop returning `ErrNoEndpoint`, or its asks stay refused.
- **States.** `adapter.State` has `accepted`, `queued`, `rejected`, `unreachable`, `unknown` (and `bounced` for mailbox entries). ADR 0012 defines `mailboxed` and `collecting`, but ADR 0012 is planned and unbuilt: no `mailboxed` state and no `MailSignal` interface exist in code. This route would be the first user of that contract, so its shared parts are built here, harness-neutral.
- **Pull filter.** `receive_message` never returns `accepted` or `queued` entries (`internal/mcpserver/server.go`, design §8). A wake that pointed at a `queued` entry would point at a message the pull hides; this is why the route returns `mailboxed` and never `queued`.
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
- **Live evidence (measured, 0.167.0).** Three successes. Two inserts into an idle thread started a turn; in one the row was removed after dispatch. In the third, one fixed wake row was inserted while the target thread was busy; the row was still present 15 s later, and the target then pulled the message with `receive_message` and replied. This matches the source's idle-only dispatch; the time the row was consumed was not measured.
- **Not verified.** "Locally loaded" scope; whether a running daemon also dispatches rows for a thread (no evidence; the watcher uses its own loaded thread manager and the writer lock gates ownership, so a double wake is a hypothesis); whether `session_id` equals `_meta.threadId`; resume, compaction and crash behaviour. The supported dedicated app-server plus `--remote` route has never been run live either.

## 3. Change (conditional: if the probe passes and the operator approves)

### 3.1 Route selection (Codex adapter, per delivery)

- App-server route when `hosts()` finds exactly one host: unchanged.
- Queue route only when all hold: the capability is turned on at install; no host is found on any endpoint; the owner proof and the claim handoff (3.3) hold at write time; the gate (3.5) passes; the delivery is `immediate` from a verified sender.
- Otherwise `unreachable` with a hint, and an ask stays `rejected/no_endpoint`.
- Asks (conditional decision): once the probe verifies the queue capability, an ask to a queue-route session is accepted in state `mailboxed`, with its route open and deadline armed as for any ask. Without that, a `--no-daemon` session stays useless for coordination. Until the probe passes, asks stay `rejected/no_endpoint`.
- `next_turn` never wakes: it stays `mailboxed` with no row. An unverified sender never wakes either, matching the existing rule that unverified senders never start a turn.
- No fallback to user input after an unknown daemon injection, and no automatic retry after an ambiguous daemon send: if the app-server route returned `unknown` for a message, the queue route is never tried for it, so one envelope cannot enter both paths.

### 3.2 States and the pull (shared, harness-neutral; ADR 0012's contract)

- The queue route's `Inject` returns `mailboxed`: the chain has already journaled the body; the only external write is the wake row. `queued` is not reused, and nothing is `accepted` before handoff.
- `receive_message` returns `mailboxed` entries (the filter keeps excluding `accepted` and `queued`). A top-level `receive_message` from the bound thread takes the per-agent mail lock, marks the returned entries `collecting` (journal-internal), restarts inform route clocks and returns the bodies with the fixed no-operator-authority notice.
- An entry becomes `accepted` on the thread's next top-level bound CrossTalk call: Codex has no pre-invocation hook here, so that call is the only evidence. This is conservative. A model that pulls and then gives a final answer may never call again, so a `collecting` entry with no evidence within a bound (a fixed time, and at exit or on replay) becomes `unknown`: readable, unresolved, never re-woken. CrossTalk does not force a tool call only to acknowledge. An ask stays open until `reply`; `reply` to an entry still `mailboxed` returns `not_collected`.
- Drain: the result of a `receive_message` that leaves `mailboxed` entries behind (a page limit, or a message that arrived during the call) says so in a fixed field, so one wake can drain several pages within its turn without a second wake. A message that arrives after the last page clears the outstanding-wake flag (3.4) may write one new row.
- The ADR 0012 extension points (`MailSignal`, the `mailboxed` state) are built once and named for no harness.

### 3.3 Registration and ownership (SessionStart, a separate must-pass goal)

- `crosstalk hook codex session-start` is stateless and never opens the journal. It hands its claim to the owning shim through an authenticated exchange and also leaves it durably (a 0600 file under `~/.local/state/crosstalk/codex-claims/`), so the handoff survives a shim that is not listening yet. It prints `{}` and exits 0 on any failure, under a short internal time bound. The claim holds `session_id`, `source`, `cwd`, `transcript_path`, the nearest ancestor `codex` pid with its start time, `CODEX_THREAD_ID` if set, and a timestamp; the latest claim per (pid, session_id) wins. It is idempotent across resume, clear and compaction refires.
- Owner and thread lineage proof: the claim's codex pid and start time equal the shim's own parent, and that parent holds `thread-writer-locks/<session_id>.lock` open (a cgo-free process file-descriptor scan). A pooled shim whose parent is `codex app-server` never matches and keeps the app-server route.
- With the proof, the shim registers the agent for that thread from the claim, without waiting for the session's own first CrossTalk call. Making that first `tools/call` a precondition would repeat today's circular dependency (the session must call CrossTalk before anyone can wake it to call CrossTalk). A later `tools/call` whose `params._meta.threadId` differs from the claimed `session_id` revokes the claim-based binding and the queue route for it.
- Without the proof, the row shows discovered and unbound only. Such a claim never unlocks a queue write, an ask, a name pin, a `claim` or `command` target, or a rebind of an existing agent record; informs to it wait in the journal or the ADR 0026 pre-bind store, with a sender hint. A claim for a `session_id` already bound elsewhere is ignored.
- Whether the owner proof is sufficient is an engineering obligation of the spike (a probe row in section 5), not an operator question.
- Acceptance check for this goal, separate from the queue route: in a prompted `--no-daemon` tab, the claim arrives, the proof holds, and a message sent before the session's first CrossTalk call is held as `mailboxed` for the right thread with no duplicate agent; in a pooled daemon session the proof fails; a forged claim from another process fails; a mismatched later `_meta.threadId` revokes. An unprompted tab is out of this check and depends on the separate lock-discovery probe.

### 3.4 The wake row

- Payload: one plain-text user input with a fixed sentence naming `receive_message` on server `crosstalk`, no sender field, no count, no message id, no peer text. A stable CrossTalk wake id rides in the row's own client id field so CrossTalk can recognize its own rows. The fetched envelope, not the wake, carries the message id, so `receive_message` stays idempotent by message id.
- Write: one `BEGIN IMMEDIATE` transaction that checks for an outstanding CrossTalk wake row for the thread and, if none, inserts at `MAX(queue_order)+1` for that thread, with a bounded busy retry; a busy timeout ends in `unreachable`, never a blind retry later. A thread at the 100-item cap gets no row and the entry stays `mailboxed`.
- At most one outstanding wake per thread. N messages make one wake; the outstanding flag is cleared by the collecting `receive_message` (3.2), or when the gate sees CrossTalk's row gone. Redundant wakes are bounded; messages are not (ADR 0014 stands: no message rate limit).
- Interrupted or paused stays paused: the 0.167.0 watcher skips interrupted threads (source), and CrossTalk does not try to defeat that; the entry stays `mailboxed` and pullable.
- Row removal at acceptance of the turn proves only that Codex took the turn, not that the model fetched anything; it is never delivery evidence.
- Write only while the owner proof holds at write time, and never when `hosts()` finds the thread on any endpoint.

### 3.5 Gate (fail closed)

- `queue_1.sqlite` already exists (CrossTalk never creates it); the table layout matches; the queue migration versions and checksums and the three revision triggers match a pinned set; the parent Codex binary's version is on a tested allowlist (0.167 only until the probe runs on others).
- Any mismatch: no write, `unreachable` with a hint that names the supported dedicated app-server route, and the message stays pullable. Checked per write, cheaply cached per process.

### 3.6 Crash windows and transport switch

- Journal commit, then queue commit, are separate. Shim dies after the journal commit and before the queue commit: the entry is `mailboxed` with no wake; the next message or a later claim handoff may write the wake. Shim dies after the queue commit and before recording it: replay finds CrossTalk's row by wake id (or none) and does not write a second; the entry stays `mailboxed`.
- Codex dies with the row unconsumed (kill -9): no SessionEnd runs; the row stays. A later `codex resume` of that thread may dispatch it; the wake is harmless by construction (fixed text, nothing to replay but a pull). Deleting CrossTalk's own stale wake rows is operator question (c); CrossTalk never deletes an unknown row or a user row.
- Queue to daemon switch: when a resumed thread appears on an app-server, the queue route stops for it; `mailboxed` entries stay pullable and are not re-sent as tool output, so one envelope never enters both paths.

### 3.7 Sender view

- The sender sees `mailboxed` with a hint: delivery is a best-effort wake that fires only when the session is idle (watcher interval 10 s at 0.167.0, not a deadline), not `delivered`. `list_agents` shows the delivery mode for the row (tool output, queue wake, or pull only) so the sender can judge an ask's deadline.
- Fetched peer text stays data under the existing notice and the skill, although the wake that prompted the fetch is a user-role turn.

## 4. Public surface to change

- Design §3: the delivery-mode field on Codex rows; the claim handoff and owner proof in registration; the discovered-unbound row.
- Design §4/§8: the `mailboxed` state in the state list; `receive_message` returns `mailboxed` entries and the drain field; ask acceptance in `mailboxed`; the sender hint text.
- Design §6: the queue route beside tool output and deferred append.
- ADR 0007 amended in place (the user-role wake exception, "no subprocess queue" narrowed); ADR 0012's state contract marked shared; ADR 0004 cross-reference; ADR 0028 new.
- An install flag (name and placement are operator question (a)), off by default, adds the SessionStart hook and turns the route on; `uninstall` removes only what it added. `install --check` gains a warn-only line for the gate.
- `docs/setup.md` (`--no-daemon` paragraph, troubleshooting rows) and the FAQ known issue: experimental, opt-in, each wake a paid turn, supported route named first.
- AGENTS.md Architecture: one clause naming the route and ADR 0028.

## 5. Commit sequence (small, test-pinned)

0. **Probe matrix (the gate; no product code).** A throwaway harness outside the product tree drives a `--no-daemon` Codex 0.167 session on a scratch thread and records, for each case, the queue table before and after, the rollout file, the TUI, and what the model did. Cases and pass criteria:
   - Idle fresh thread: one wake, one dispatch, row removed; time from insert to dispatch recorded.
   - Busy mid-turn: the row waits and dispatches once after the turn, never steering mid-turn; consumption time recorded (the third live success did not measure it).
   - Interrupted: the row does not dispatch while interrupted and the message stays pullable.
   - Resumed (same and other process): the wake fires at most once in total.
   - Compaction: the wake neither replays nor duplicates; SessionStart refire is idempotent.
   - kill -9 at consumption: no second dispatch of an already-consumed wake on resume; pending mail still pullable.
   - Two wakes queued (forced, to test the guard's absence): observe order and duplicate turns.
   - Operator draft in the composer: the draft is not lost, submitted or merged with the wake.
   - Operator's own queued prompt present: order preserved, the operator's item never reordered.
   - Competing ownership and ownership transfer (replaces "daemon double wake", a hypothesis with no evidence): a daemon and a `--no-daemon` process on the same thread in turn; only the writer-lock holder dispatches, and a transfer neither drops nor doubles the wake.
   - Owner-proof sufficiency: a forged claim from a sibling process, a pooled daemon shim, pid reuse after the owner exits, and a lock held by another process each fail the proof.
   - Hook `session_id` equals `_meta.threadId` on the same thread, across startup, resume and fork.
   - Tab-open discovery (separate probe): whether a parent-owned writer lock alone identifies an unprompted tab's thread.
   - Acknowledgement: a model that pulls and gives a final answer with no further call; the entry reaches `unknown` at the bound and is not re-woken.
   - Model treats fetched text as data: a fetched body that asks for an action is reported, not acted on.
   Reject the route if any case shows peer text becoming operator input, a wake replaying unintended work, or pending mail disappearing. Report the matrix to the operator before step 1.
1. **SessionStart claim handoff and owner proof.** Hook command, authenticated exchange, durable claim, lineage check, revoke on mismatch; tests for forged claim, pooled shim, refire idempotence, unproven-claim limits, a message held before the first CrossTalk call. Must pass on its own; the queue route does not start without it.
2. **Shared `mailboxed` / `collecting` states.** Journal records, pull filter, settlement on next bound call, the acknowledgement bound to `unknown`, `not_collected`, replay to `unknown`; harness-neutral tests with `adapter.Fake`.
3. **Gate.** cgo-free SQLite read of schema, migrations, triggers; version allowlist; table test against fixture databases (matching, extra migration, missing trigger, missing file).
4. **Queue write.** One transaction, outstanding-wake check, wake id, busy retry bound, the 100-item cap; tests against a fixture database for concurrent writers, the outstanding guard, and no row on any gate failure.
5. **Route selection and states in `Inject`.** `next_turn`, unverified sender and post-`unknown` daemon cases never wake; asks accepted `mailboxed` behind the probe result; crash-window and transport-switch tests.
6. **Sender hint, delivery mode, install flag, `install --check` line, docs, ADR 0007 and design edits.**

Every implementation commit carries `Provisional-ADR: 0028`.

## 6. Risks

- **Private store, no contract.** A later migration, a validation change or a JSON decoding change can make the row invalid or change dispatch. At 0.167.0 an invalid row is deleted and the loop continues (source), so the row cannot block the operator's queue at that version; a later version is covered only by the per-write gate and the allowlist.
- **User-role authority.** The wake shows as a user turn. Any same-user process could already write this database; CrossTalk adds a fixed sentence, never peer text. The fetched body still arrives as tool output with the notice; the probe checks the model keeps treating it as data.
- **Spend and noise.** Each wake starts a paid model turn and is visible to the operator. Coalescing bounds wakes per thread, not messages. Opt-in only.
- **No consumption evidence.** Codex removes the row when it accepts the turn, before any fetch; the model may ignore the wake or stop on an approval prompt, and a model that pulls and ends its turn leaves the entry `unknown` at the bound. The sender's hint never claims delivery.
- **Coalescing suppressing a needed wake.** If the outstanding flag is cleared wrongly, a wake is lost; if never cleared, later mail waits for a manual pull. The flag is cleared only by collection and by the observed absence of CrossTalk's own row.
- **Queue cap.** A thread with 100 queued items gets no wake.
- **Stale rows** after a crash survive in Codex's database until resume or deletion.
- **Identity.** `session_id` equal to the thread id is assumed until the probe; a wrong assumption fails closed (no bind, no write).
- **cgo-free SQLite** must handle WAL and Codex's locking correctly; a driver that does not is a corruption risk to the operator's queue.

## 7. Open questions for the operator

The operator asked for this experiment, so whether to write Codex's queue at all is not asked here; the probe decides feasibility. The choices left to the operator:

- (a) Opt-in default and where the flag lives: off by default is proposed; the flag's name and whether it sits on `crosstalk install`, in config, or both.
- (b) Wake-turn spend: is a fixed-text user-role wake that starts a paid model turn accepted as an exception to ADR 0007/0004?
- (c) May CrossTalk delete its own stale wake rows (identified by wake id; never an unknown row or a user row)?

## 8. Decided

Nothing is decided by the operator. Recorded from the council and coordinator, binding on any build, all conditional on the probe: spike first; experimental and off by default; journal authoritative; ADR 0012 states reused, no `accepted` before handoff; acknowledgement on the next bound call, else `unknown` at a bound, no forced tool calls; queue writes only in the Codex adapter; fail-closed gate; one outstanding wake per thread; `next_turn` and unverified senders never wake; no fallback to user input and no automatic retry after an ambiguous daemon send; asks accepted `mailboxed` once the probe verifies the capability; registration from an owner-proven claim, not from the first `tools/call`; honest sender state; no message rate limit, bounded redundant wakes; never delete unknown or user rows; no broad SQLite abstraction; cgo-free driver; no upstream Codex issue without the operator.

## 9. Rollout

Behind an install flag, off by default (operator question (a)), labelled experimental in the install output, the setup page and the `list_agents` delivery mode. A session without the flag behaves exactly as today (`unreachable`, `no_endpoint`), pinned by a test. Removing the flag removes the hook and stops queue writes; `mailboxed` entries stay pullable. A Codex upgrade outside the allowlist turns the route off by the gate, with the hint, until the probe is re-run for that version.

## 10. Codex review

Not yet run. Per AGENTS.md the review is an `ask` to the standing Codex reviewer session naming this worktree and base `main`; its findings are fixed or rejected here with a reason before the operator reviews the plan.
