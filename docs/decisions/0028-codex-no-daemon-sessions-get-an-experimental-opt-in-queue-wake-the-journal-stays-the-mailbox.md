# ADR 0028 — Codex no-daemon sessions get an experimental opt-in queue wake, the journal stays the mailbox

- **Status:** Provisional (council 2026-11-10; design only, the operator has not approved a build, gated on the probe matrix)
- **Date:** 2026-11-10
- **Deciders:** operator (pending) + Claude + council (five advisors, one of them Codex)
- **Implementation:** planned, not built. No hook command, no claim file, no `mailboxed` state and no SQLite driver exist in the code.
- **Context source:** issue #112; [change plan](../plans/codex-no-daemon-queue.md); council record of 2026-11-10

## Context

A Codex session started with `codex --no-daemon` runs a private app-server inside its own process, with no socket CrossTalk can dial. It can send and pull messages but cannot receive them: `Register` binds it with an empty endpoint, `Inject` returns `ErrNoEndpoint`, a send is `unreachable` and an ask is `rejected/no_endpoint`. The supported workaround, a dedicated app-server plus `codex --remote` (`docs/setup.md`), has never been run live.

The only inbound surface such a process has is its local queue, `~/.codex/queue_1.sqlite`, table `queued_items`, which holds the user's queued prompts and which a watcher in the Codex process dispatches into the thread. Since a 2026-08-17 Codex migration, revision triggers make an external insert visible to that watcher. Codex source at tag `rust-v0.167.0` (`codex-rs/ext/queue/src/service.rs`, read, not run) shows a 10 s watcher that skips Running, Interrupted, Shutdown and NotFound threads, deletes an invalid or non-user row and continues, and removes a row when Core accepts the turn, before any proof the model fetched anything; the queue holds at most 100 items. Three live inserts on 0.167.0 worked: two into an idle thread started a turn; one into a busy thread was still queued 15 s later, after which the target pulled the message and replied. The queue has no external-write contract and a dispatched row is a user turn. Whether a running daemon competes for the same thread is a hypothesis with no evidence; the writer lock gates ownership.

ADR 0007 keeps peer text out of operator input and says no subprocess queue is used; ADR 0004 keeps peer text out of instructions. ADR 0012 defines `mailboxed` and `collecting` for a harness with no push channel, but is unbuilt. `receive_message` never returns `accepted` or `queued` entries, so a wake that pointed at a `queued` entry would point at nothing the model can pull.

A SessionStart hook fires at the first turn, not at launch (measured), and the shim starts about 1 s after Codex. An unprompted tab fires no hook. Today a session is unreachable until its own first CrossTalk call, and nothing can wake it to make that call. The owning Codex process holds `~/.codex/thread-writer-locks/<thread>.lock`; the shim's parent is that process; pooled daemon shims have a different parent. The hook's `session_id` is self-asserted stdin text.

## Decision

If the probe matrix in the plan passes and the operator approves, a `--no-daemon` session may receive through a queue wake, built as follows. Until then nothing is built.

1. **Experimental and opt-in.** Off by default, turned on by an install flag, never described as production-ready. The supported routes (daemon, dedicated app-server) are unchanged and named first in the docs.
2. **The journal is the mailbox.** The receiver chain is unchanged (authenticate → dedupe → hop-chain → policy → `Inject`). The queue write is an outcome of the Codex adapter's `Inject`, which returns `mailboxed`. ADR 0012's states are reused and built harness-neutral: `receive_message` returns `mailboxed` entries and marks them `collecting`; the next bound call from that thread settles them `accepted`. A model that pulls and then ends with a final answer may never call again, so no evidence within a bound (and at exit or replay) means `unknown`: readable, unresolved, never re-woken; CrossTalk never forces a tool call only to acknowledge. Nothing is `accepted` before handoff, and `queued` is not reused. Row removal by Codex is never delivery evidence.
3. **The wake carries no peer text.** One row with a fixed sentence naming `receive_message` on server `crosstalk`: no sender, no count, no message id, plus a stable CrossTalk wake id in the row's client id field. The body arrives as tool output with the existing notice and stays data under the skill, although the wake itself is user-role.
4. **Bounded writes.** At most one outstanding wake per thread, checked and inserted at `MAX(queue_order)+1` in one immediate transaction with a bounded busy retry; a thread at the queue cap gets no row. A pull that leaves entries behind says so, so one wake drains several pages; a message arriving after the drain may write one new row. There is still no message rate limit (ADR 0014); only redundant wakes are bounded.
5. **Fail closed.** Write only when the file already exists, its table layout, queue migration checksums and revision triggers match a pinned set, and the parent Codex version is on a tested allowlist. Otherwise `unreachable` with a hint naming the supported route, and the message stays pullable. Never write when any app-server hosts the thread.
6. **Who never wakes.** `next_turn` (mailbox only), unverified senders, and a message whose app-server injection came back `unknown` (no fallback to user input and no automatic retry, so one envelope never enters both paths). An interrupted or paused thread stays paused.
7. **Asks.** Once the probe verifies the queue capability, an ask to a queue-route session is accepted in state `mailboxed`, route open and deadline armed; until then it stays `rejected/no_endpoint`.
8. **Registration and ownership.** Registration is its own must-pass goal: a session is receivable before its first message is sent to it, and once SessionStart has fired it no longer depends on the session's own first CrossTalk call (making that call a precondition would repeat today's circular dependency). A stateless SessionStart hook hands a claim to the owning shim through an authenticated exchange and leaves it durably in a 0600 file. The shim registers from the claim only when owner and thread lineage are proven: the claim's pid and start time are the shim's parent, and that parent holds the thread's writer lock. A later `tools/call` whose `_meta.threadId` differs revokes it. Without the proof the row is discovered and unbound only, and the claim never unlocks a queue write, an ask, a name pin, a `claim` or a `command` target. Whether the proof is sufficient is a probe row, not an operator question. An unprompted tab fires no hook; discovering it from parent-owned locks is a separate probe and not promised.
9. **Honest sender state.** The sender sees `mailboxed` with a hint (best effort, idle-only, a 10 s watcher interval at 0.167.0 per source, not a deadline), never delivered; `list_agents` shows the row's delivery mode. Crash windows across the two stores and the queue-to-daemon switch after a resume are specified in the plan, section 3.6.
10. **Small footprint.** No general SQLite abstraction; a cgo-free driver, so the signed static release holds (ADR 0015, 0017). No upstream Codex issue or request without the operator.

When built, ADR 0007 is amended in place: the fixed-text user-role wake is a named exception, and "no subprocess queue" is narrowed to "no peer text in a queue".

## Rejected alternatives

- **Peer text in the queue row.** Gives peer text operator authority (ADR 0007, 0004) and would replay with the row on resume.
- **Returning `queued` for a written row.** The pull hides `queued` entries, and a committed row says nothing about the model.
- **On by default, or production status now.** Three successes on one version; the interrupted, resumed, compaction, crash and ownership-transfer cases are untested; each wake is a paid turn.
- **A general wake primitive for every harness.** Premature before one harness's queue lifecycle is measured.
- **Trusting the hook's claim alone.** Its `session_id` is self-asserted; any same-user process could write a claim for a thread. The owner and lineage proof is the minimum.
- **Requiring the session's first `tools/call` before the queue route opens.** Circular: nothing can wake the session to make that call.
- **Daemon-only (do nothing).** Leaves #112 open on an untested workaround. It stays the default, and the answer if the probe or the operator says no.
- **Deleting or rewriting rows CrossTalk did not write, or unknown rows.** Never. Deleting CrossTalk's own stale wake rows is operator question (c).
- **A broad SQLite layer or a cgo driver.** More surface than one guarded insert needs; a cgo driver breaks the static signed release.

## Consequences

- The route depends on a private Codex store and on observed, not contracted, behaviour. Each Codex minor needs the probe re-run before the allowlist grows; until then the gate turns the route off.
- Each wake costs a model turn and is visible to the operator as a user turn.
- A crash can leave a CrossTalk wake row in Codex's database; a later resume may fire it once. The wake is harmless by construction (it only asks for a pull), not by cleanup.
- An unprompted `--no-daemon` tab stays unreachable; the docs must say so.
- Building it also builds ADR 0012's shared `mailboxed` contract, which changes `receive_message` and the state list in design §8, and needs edits to design §3, §6, ADR 0007, `docs/setup.md`, the FAQ and AGENTS.md.
- Implementation commits carry `Provisional-ADR: 0028`.

## Open questions (operator)

The operator asked for this experiment; whether to write the queue at all is decided by the probe, not asked.

- (a) Opt-in default and where the flag lives (off by default proposed).
- (b) Is the wake-turn spend, a fixed-text user-role wake that starts a paid model turn, accepted as an exception to ADR 0007/0004?
- (c) May CrossTalk delete its own stale wake rows (never an unknown row or a user row)?

## Trial record

- Council verdict: a bounded feasibility spike first, then at most an experimental opt-in capability (chair's synthesis, accepted by the coordinator). Sat: five advisors over two rounds, four Claude and one Codex; all rated the Codex executor's position strongest.
- Rests on: ADRs 0004, 0007, 0008, 0012, 0014, 0026 (provisional).
- Commits: `git log --grep 'Provisional-ADR: 0028'` (none).
- Pinning test: none yet; the probe matrix (plan section 5, step 0) is the gate.
- Outcome: not run.
