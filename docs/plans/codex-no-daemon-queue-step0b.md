# Step 0b results: isolated prototype of the `--no-daemon` queue-wake semantics (#112)

Plan: [codex-no-daemon-queue.md](codex-no-daemon-queue.md), section 5 step 0b. ADR: [0028](../decisions/0028-codex-no-daemon-sessions-get-an-experimental-opt-in-queue-wake-the-journal-stays-the-mailbox.md). Step 0 report: [codex-no-daemon-queue-step0.md](codex-no-daemon-queue-step0.md). Run 2026-11-10, Go 1.27.3, macOS arm64. The code is in `proto/codexqueue/` (its own package, stdlib only, nothing imported from or into `cmd/` or `internal/`). No Codex was run in this step.

## Bottom line

- **No reject condition was seen.** Nothing in the prototype lets peer text reach operator input, lets a wake replay unintended work, or lets pending mail disappear. Every wake is a fixed row; every entry is pullable before any row exists; a lost wake never loses mail.
- **68 tests pass** (`go test -race ./proto/...`; 16 more are route-table subtests), `go vet` clean, gofmt clean. Six `TestReal_*` tests use the real kernel on this host (a non-blocking `flock`, and for four of them the `lsof` opener list, against child processes standing in for the Codex process); five of the six spawn children, and one (missing lock file) spawns none.
- **The plan's state machines hold as written, with open clarifications** (section "Plan and ADR changes for steps 1 to 7"). The contradiction between "revoke on a foreign claim" and "ignore a foreign claim" is settled by the coordinator (ignored; revoke only by recheck) and fixed in plan 3.3 and ADR 0028 item 8. Still open: the fate of a reply that arrives after `unknown`, what "readable" means for `unknown` entries, the owner-proof inference's transient-opener behaviour, and journal growth from `failed` generations.
- **The owner-proof inference works on a real kernel** and fails closed on every second-opener case tried.

Terms. Entry: one message in a thread's mailbox. Generation: one wake row's lifecycle record (plan 3.4). Owner proof: plan 3.3's obligations. Verdict words: **holds** (works as the plan says), **needs plan change** (works, but the plan text must change), **unknown** (not established here). The Codex simulator in the tests encodes only what step 0 measured; it is not new evidence about Codex.

## Pre-registration

Each test's expected result is in its name (`Test<Area>_<Situation>_Expect<Result>`), written before the implementation; the route table and the claim cases are tables of expected rows. The first full run after the implementation passed. Test strength was then checked by mutation (a deliberate one-line break of the code, to see whether a test fails), and an independent verification found gaps that were fixed: the unissued-cursor test used a nonexistent entry id (now a real entry no pull issued); no test covered `unknown` becoming `accepted` (now pinned, named as pending Clarification A); the concurrent pull-and-list test depended on scheduling (now deterministic with channels, 200 of 200 runs fail under a "list settles" mutation); the route test never varied `AsksEnabled` and compared `Select` with `Matches`, which could not fail (now compared with an independently written oracle). Mutations now caught: removing the second-opener rule (4 tests), the monotonic rule, the issued-cursor check, `unknown` to `accepted`, a list call that settles, and ignoring `AsksEnabled`.

Test-strength limits: the mailbox, generations, claims and route are in-memory models written by the same author as the tests, so they pin the plan's rules, not an independent implementation; the concurrency tests prove the settle rule ignores non-evidence calls, not how the real MCP server orders calls; the real-kernel tests cover one host (macOS, same user, `lsof`); and no mutation run covered the wake-generation or replay code beyond the named tests.

## Results per item

| # | Prototype item | Tests | Verdict |
|---|---|---|---|
| 1 | Wake generation lifecycle and ambiguity policy | 17 | holds; one clarification (journal growth) |
| 2 | Mailbox committed before any wake row | 6 (router) | holds |
| 3 | Causal delivery settlement | 14 | holds; two clarifications |
| 4 | Concurrent pull and `list_agents`, response withheld | 1 (race detector on) | holds |
| 5 | Per-(harness, thread) claims, two threads in one process | 6 | holds; one contradiction in the plan text |
| 6 | Owner-proof inference and its failure cases | 12 fake + 6 real-kernel | holds, with stated limits |
| 7 | Route table, prior-unknown guard, ambiguous owner | exhaustive partition + 15 rows | holds |
| 8 | R183 (a) and (b) | 2 | holds (accepted outcomes) |

### 1. Wake generations: holds

Built: `Wakes` with the six states, a persisted per-thread generation list, the open-generation guard, a bounded busy retry, the cap and existing-row refusals, `Observe` (row gone: `admitted`), `Pulled` (`collected`), `Tick` (admission bound: `unknown`) and `Replay` (never writes a row).
Results as pre-registered:

- A second signal while a generation is `queued` or `admitted` writes nothing, including a message arriving in the gap after Codex took the row.
- A pull closes the generation `collected`; the next arrival opens generation 2.
- `admitted` with no pull stays `admitted` at 29 s and becomes `unknown` at the 30 s bound; ticks alone write no row; each later arrival costs exactly one row (no loop).
- Busy twice then commit: `queued` inside the retry bound. Busy past the bound, or the queue at its 100-item cap: `failed`, no row, and neither a tick nor a replay retries.
- Replay: `pending` with no row becomes `unknown` and writes nothing; `pending` with the row present becomes `queued` (no second row); the same crash window where Codex already took the row becomes `unknown`; `queued` with the row present stays `queued`; `queued` with the row gone becomes `admitted`, then `unknown` at the bound; replay alone opens no generation.

Clarification: a `failed` generation is journaled for every arrival that cannot write (a persistently busy database, a full queue). That is bounded in cost (no paid turn) but unbounded in journal rows. The guard test that forces "an earlier CrossTalk row is still in the table" cannot be reached by the state machine as specified (a row still present keeps its generation `queued`); it is a defensive check against a lost journal, not a live path.

### 2. Mailbox before wake: holds

Built: `Router.Receive` commits the mailbox entry, then calls `Wakes.Signal`; a crash point sits between them.
Results: at the moment the fake queue sees the insert, the entry is already `mailboxed` (snapshot taken inside the insert); a pull immediately after the row commits returns the entry; a crash before the signal leaves a pullable entry with no generation, and the next arrival opens one whose single wake drains both entries; a route without a wake (row 4) writes nothing; a failed write leaves the entry `mailboxed`.

### 3. Causal settlement: holds, two clarifications

Built: `Mailbox` with `mailboxed`, `collecting`, `accepted`, `unknown`; the monotonic rule as an allowed-transition table; per-pull tokens; cursors recorded as issued; `Reply`, `Ack`, `Expire`, `ReplayAfterCrash`.
Results as pre-registered:

- A pull marks `collecting` and issues a token. A late `mailboxed` outcome after `collecting`, or any outcome after `accepted`, is ignored; `mailboxed` to `accepted` without a pull is ignored.
- Settled `accepted` by exactly three things: a `reply` naming the entry, a later pull whose `after` is a cursor an earlier pull issued and that covers the entry, or an echoed token. A second pull with no cursor, a cursor no pull issued, a wrong token, and every call from another thread settle nothing.
- `reply` to a `mailboxed` entry returns `not_collected`, and to another thread's entry returns not found.
- No evidence: still `collecting` at 59 s, `unknown` at the 60 s bound; replay after a crash turns `collecting` into `unknown` and leaves `mailboxed` entries alone.
- A page limit sets `more`; the second pull's cursor settles the first page `accepted` and collects the rest.

Clarification A: a `reply` that arrives after the entry went `unknown` leaves it `unknown` (the monotonic rule as written treats `unknown` as terminal), although a reply is causal evidence of receipt. The plan should say whether `unknown` can still become `accepted` on a late reply.
Clarification B: the plan says `unknown` is "readable", but the pull returns only `mailboxed` entries, so nothing in the tool surface reads an `unknown` entry again. The plan should say how a model or a sender reads one (a pull flag, or `list_agents`, or not at all).

### 4. Concurrent pull and `list_agents`: holds

Test: the pull's response is withheld by blocking inside a hook after the entries are marked `collecting`; `list` calls (which take the same settle path as every call, with no evidence) run while it is blocked, and in a second order before the pull starts; the entry is `collecting` throughout and `unknown` at the bound. A separate race-detector test runs eight concurrent lists with a pull. Limit: the "concurrent call" is a method on the same mutex-guarded type, not the real MCP server; it proves the settlement rule ignores such calls, not that the server orders them as modelled.

### 5. Per-thread claims: holds, one contradiction in the plan text

Built: `Shim` with a claim per session id, `AcceptLive` (transport pid and its ancestry), `AcceptDurable`, `Recheck`, per-request `Call`.
Results: two claims in one shim (the `/clear` case: new session id, same process, old lock still held) coexist and survive a recheck; revoking one thread (its lock released, or a second opener appearing) leaves the other bound; a changed parent start time revokes both; a per-request thread id with no claim stays unbound and creates no claim, and for a claimed thread it confirms the claim; 150 concurrent calls across two claimed threads and one unclaimed thread, then a concurrent recheck while one thread is revoked, never cross-bind or cross-revoke (race detector on); a refire (`startup`, `compact`, `resume`) leaves one binding.

Contradiction: plan 3.3 says a claim "for the same `session_id` arrives from a different process" revokes the binding, and also says "a claim for a `session_id` already bound elsewhere is ignored". The prototype takes the second reading: a claim that fails the proof (outside the parent's ancestry) is ignored and revokes nothing; revocation happens only through the recheck. The first reading would let any same-user process revoke a proven binding with a forged claim, and in `--no-daemon` each process has its own shim, so a competing claim reaches a different shim that cannot revoke this one's binding.

### 6. Owner-proof inference: holds, with limits

Built as the step 0 inference: lock held by someone (non-blocking `flock` fails with EAGAIN), the shim's parent has the lock file open, and no other process has it open. Plus the live-ancestry check (transport pid up to the parent, matching pid and start time), the pooled-parent check, and the durable-claim path with the target check only.
Fake-system results (12): owned thread bound; a body pid, start time or `CODEX_THREAD_ID` that disagrees with the transport is ignored; a body pid that matches the parent cannot stand in for transport ancestry; pid reuse (same pid, different start time) rejected; a sender outside the ancestry rejected; a durable claim for a thread the parent does not own rejected (lock free); lock held by another process while the parent has no fd rejected; the parent has the file open but the lock is free rejected; a second opener rejected; a pooled parent (`app-server` in its arguments) rejected; a second opener that appears after binding revokes it on recheck.
Real-kernel results (6 `TestReal_*` tests, macOS, this host): `flock` on a free file reports free, on a file a child holds reports held, and after `kill -9` of the holder reports free again; a missing lock file is an error, not "free"; with a child holding the lock and being the parent, the proof passes; with an added reader process that only opens the file, the proof fails with the second-opener error; with the parent only reading while another process holds the lock, it fails the same way; with the lock free and the parent only reading, it fails as lock-free.

Limits, stated rather than hidden:

- **Transient openers fail the proof.** Any second opener of the lock file (a backup tool, an indexer, `lsof` itself if not excluded, an operator `cat`) fails the proof for as long as it is open, and a recheck at that moment revokes the binding. Fail closed is safe, but the route would flap. A product recheck should debounce (revoke only after two failed checks a short interval apart) and the docs should say so. Not built or tested here.
- **Two reads, not one atomic read.** The lock check and the opener list are separate observations; a holder or opener changing between them is not excluded. The plan already rechecks at write time inside the write path; the proof is as strong as that recheck, no stronger.
- **The opener list is same-user only.** `lsof` does not show other users' processes without root. That is consistent with ADR 0004's single-user trust model, and the docs should say the inference assumes it.
- **Cost.** The `lsof` shell-out measured about 0.17 s per call in these tests. The product needs either a raw `proc_info` call (cgo-free, no shell-out) or an accepted per-write cost; step 0 already noted the `proc_info` question. Ancestry and parent start time were not re-measured here (step 0 measured the cgo-free `sysctl` reads); they are faked in the tests.
- The durable-claim path keeps the ADR 0004 limit: it proves only a thread the parent owns, not that the claim came from the parent's hook, and no test claims otherwise.

### 7. Route table: holds

Built: `Select` and `Matches` over the plan 3.1 inputs. An exhaustive loop over every combination of prior-unknown, 0 to 3 hosts, 0 to 3 claim processes, setting, gate, immediacy, sender verification and ask (2048 inputs, with `AsksEnabled` varied, compared with a separately written oracle) shows each input matches exactly one row, a prior `unknown` matches only row 1, and no other input matches row 1. Fifteen named cases pin each row: a prior `unknown` beats a host and beats full capability; one host and no claim is the unchanged app-server route; setting off, no claim, a failing gate, `next_turn` without capability and an unverified sender without capability are all row 3 (`unreachable`, an ask `rejected/no_endpoint`), never silently `mailboxed`; `next_turn` and an unverified sender with capability are row 4 (mailbox, no wake); an immediate verified sender with capability is row 5 (mailbox and wake); two hosts, a host plus an owner-proven claim, and claims from two processes are row 6. An ask with capability but asks not yet enabled is row 3, matching "never reported present for an ask".
Note: the guard's input, "an earlier attempt ended `unknown`", is per message; the prototype takes it as a boolean, so the product needs a per-message lookup in the journal.

### 8. R183 (a) and (b): holds (accepted outcomes)

(a) With the user's follow-up typed into the simulated TUI queue and a wake row inserted: the table holds no row for the follow-up; the watcher dispatches the wake as a queue turn and folds the follow-up into that turn's inputs; the follow-up does not also run as its own turn; the generation and the mailbox behave exactly as without it (`admitted`, then `collected` on the pull, entry pullable). No CrossTalk state depends on it, so it is not a reject condition.
(b) With a row consumed and the process killed at that moment: no redelivery on resume; the generation goes `admitted`, then `unknown` at the bound with no row written; the entry stays `mailboxed` and the pull returns it; the next arriving message opens generation 2 and writes one row. The sender sees `mailboxed` throughout. Consequence the docs must state: if the lost wake was the only message, nothing wakes the session again, and an ask's deadline expires; the mail waits for a manual pull.
Limit: the simulator reproduces the two step 0 measurements (probes 6 and 9); it does not test Codex.

## What was not built or not tested

- The real journal, the real receiver chain, the real MCP server and the real shim: all in-memory or modelled. Integration is steps 1 to 5.
- The gate (plan 3.5, step 3) and the real SQLite write (step 4): the queue is a fake with the cap, busy and existing-row behaviour; concurrent writers, WAL behaviour and a cgo-free driver are untested.
- Live-handoff ancestry from a real hook process, and `sysctl`, `LOCAL_PEERPID` reads: measured in step 0, faked here.
- Subagent threads and the step 0 item 11 leftovers (fork, a pending approval prompt, a no-notice fetched-text run, the dedicated app-server route): not in 0b.
- Real Codex, any hc terminal, the real `~/.codex`, and any real database: not touched.

## Plan and ADR changes for steps 1 to 7

1. **Plan 3.3 revocation, ADR 0028 item 8 (done in this branch, coordinator ruling):** a foreign claim is ignored; revocation only by recheck. Still open: add a recheck debounce (two failed checks before revoking) and say the opener list is same-user only.
2. **Plan 3.2, ADR 0012 item 3 narrowing:** decide late evidence after `unknown` (recommended: a `reply` or an echoed token after `unknown` may move it to `accepted`, because both are causal; the monotonic rule then reads "never back to `mailboxed` or `collecting`"), and decide how an `unknown` entry is read (recommended: a pull flag that returns `unknown` entries without changing state).
3. **Plan 3.4 and 3.6:** state that `failed` generations are journaled per attempt and may be coalesced (one `failed` record per thread until a success), and state the admitted bound's value as a named constant to be set at step 4. Add to the sender hint and the docs the two R183 consequences: a wake turn may carry the user's TUI-queued prompt (R183 (a)), and a crash can lose the only wake so an ask may time out (R183 (b)).
4. **Plan 3.3 and step 1 tests:** replace the `F_GETLK` language in every remaining place with the inference (done for obligation 4 in this branch's first commit; check design §3 at build). Add test cases for the transient reader and the debounce. Prefer a raw `proc_info` opener read over an `lsof` shell-out, or record the shell-out as a `ponytail:` ceiling.
5. **Plan step 2 (minor):** plan 3.2 already says cursors come only from pull results; the prototype enforces it by tracking issued cursors, and a real entry id no pull issued settles nothing. Worth one clause in step 2's test list, no design change.
6. **Plan 3.1 and step 5:** the prior-`unknown` guard is per message; the journal needs a per-message "any route ended `unknown`" lookup that the route table reads.
7. **ADR 0007 and ADR 0012 item 3:** unchanged from the plan's own list; wording changes at build, not now.
8. **ADR 0028 Trial record:** record the step 0b outcome (68 tests, no reject condition, the open clarifications above) when the plan is next revised.
