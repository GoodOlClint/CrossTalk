# Change plan: role inboxes (#54, ADR 0024) with the pre-bind store (#41, ADR 0026)

Status: draft for operator review (Codex review dispositions in section 10). No code is written; this plan is the brownfield gate. Branch `plan-54-inboxes` from `main` at c1a4cf7.

## 1. Goal and scope

Build, in one stacked series:

1. **Role inboxes** (ADR 0024, issue #54): `attach`, `detach`, `grant`, the `inbox:<name>` address, `claim("inbox:<name>")`, fan-out with one responder, the fallback walk, membership notices, `list_agents` visibility.
2. **Pre-bind store** (ADR 0026, issue #41): a message delivered to a session before it binds is kept and replayed when it binds, so `reply` finds it.

Operator rulings this plan rests on (2026-11-09): build ADR 0024 as designed, not the docs-only route; fold #41 into the same build; #54 and #41 close when both land.

Out of scope, named: the offline spool (a message to a session or inbox that is not running; ADR 0024 "Offline delivery: sketched"), revoking a grant, `attach` without taking over the responder role, cross-host inboxes, the pre-bind `agent_id` difference noted under #43.

The two tracks share no behaviour. They share files (`internal/shim/send.go`, `internal/shim/agent.go`, `internal/receiver/receiver.go`, `internal/journal/journal.go`), so they are stacked in one series, not run in parallel.

## 2. Existing state (what the code does today, checked on main)

- **Names and targets.** `ResolveTarget` / `resolveLocal` (`internal/mcpserver/target.go`) pick the lookup by shape: agent_id-shaped values match agent_ids, anything else matches a registered name, then a native name. `SplitAddress` splits `name@alias` for paired hosts. Nothing handles an `inbox:` prefix, so `inbox:x` today resolves as a name and is `no_such_agent`.
- **Tools.** Seven tools registered in `Server.addTools` and `addClaimTools` (`internal/mcpserver/server.go`, `claims.go`). `claim`/`release` take one `name` argument (`nameIn`), resolve it with `claimTarget`, and apply the claim in the target's own journal (`Agent.applyClaim`, which also republishes the descriptor with a changed role). A subagent caller gets `rejected/caller_not_primary`. `boundTool` gives an `unbound` error to unidentified callers.
- **Descriptor.** `registry.Descriptor` (`internal/registry/descriptor.go`) has no inbox field. `Registration.Publish` owns the identity fields and copies the rest from the caller's descriptor; `register()` in `server.go` builds a fresh descriptor after journal replay. `readDescriptor` uses plain `json.Unmarshal`, so an older reader ignores an added field.
- **Sending.** `AgentState.deliver` (`internal/shim/send.go`, 110 lines) resolves the target, then branches: remote, registered (journal outbound → `viaTransport` → settle), or native inform (`nativeTarget` + adapter inject). `newMessage` fixes `From`, `HopChain`. `countSend` keys the nudge by peer.
- **Journal.** One JSONL per agent (`internal/journal/journal.go`); record types dedupe, task, dispatch, outcome, cancel, route, obligation, claim, outbound, receipt, nudge. Latest-by-key semantics; `unresolved()` decides what survives the 24 h retention window. `TypeClaim` is retained while `Claimed`. The outbound record is keyed by `message_id` alone.
- **Receiver chain.** `Receiver.receive` (`internal/receiver/receiver.go`): validate → `AcceptRecord` (dedupe) → admission (hop, policy, size) → reserve → receipt → dispatch claim → open route → inject → close route on `rejected`/`unreachable`. `Recover` restores routes from `journal.Replay` with the journaled `At`. `openRoute` stamps `r.now()`.
- **Reply.** `Shim.Reply` looks up the dedupe record by message id; a miss returns `no_such_message` with the pre-bind hint when the id's UUIDv7 time is before `boundAt`.
- **Pre-bind today (ADR 0023 middle path, built).** A native inform carries `from-agent-id` (Claude) / `from_agent_id` (Codex); `hintNativeOneWay` and `hintNoRoutePreBind` say the answer comes as a new `send_message`. No store exists. `internal/wire/wire.go` has the per-agent token path `tokens/<agent_id>.token`.
- **Durable route close (#63) is already shipped** (PR #123, `close_order_test.go`: `TestRouteClosedBeforeUnreachableRecorded`, `TestFailedRouteCloseIsUnknownNotUnreachable`). `receiver.go` closes the route before it records `unreachable` and reports `unknown`/`journal` when the close fails.
- **Message type.** `msg.Message` has no metadata field. A2A carries per-message extras under `metadata.crosstalk` (`MetaKey`, `internal/a2a`).
- **Remote.** The gateway row `remote.AgentRow` is an allowlist struct, so a new local descriptor field never reaches a paired host.

## 3. Where ADR 0024 has drifted from the code

| # | ADR 0024 says | Code today | Plan |
|---|---|---|---|
| D1 | Rule 5 "Durable route closure" and Consequences "Receiver-wide change": `closeRoute` ignores a failed append; the outcome is written before the close. | Shipped in #63 / PR #123. | Drop the commit. Edit ADR 0024 in place to say so. Reuse the two existing tests as the walk's precondition; add one test (I8) that the fallback walk stops on `unknown`/`journal`. |
| D2 | Rule 6 and Consequences: the copy marker is `metadata.crosstalk.inbox_copy`, "persisted on the task and journal records". | `msg.Message` has no metadata. The marker exists only on the A2A wire. | Keep `msg.Message` unchanged. The A2A server maps the wire metadata to two new fields on `receiver.Inbound` (`Inbox`, `InboxCopy`); `journal.Record` gains `Inbox` and `Copy`, written on the dedupe and task records. Design §4 is described as A2A metadata only. No ADR text changes. |
| D3 | Rule 6: a copy's A2A task "completes on delivery" and is admitted outside `MaxOpenAsks`. | `tasks.create(who, m, wire, MaxOpenAsks, MaxPending)` (`internal/a2a/tasks.go`) counts every ask. | I7 passes the copy flag into `create` so the ask limit skips it. Matches ADR. |
| D4 | Rule 3: "the resume replay of rule 11" under the inbox lock, "republish the inbox in the descriptor". | `register()` builds the descriptor after `journal.Open` and before `Bind`/`Publish`. | I4 does the replay inside `register()` between journal open and `Publish`. Matches ADR; location named. |
| D5 | Files list says `internal/shim/send.go` "fan-out in `deliver`". | `deliver` is one 110-line function whose registered branch is inline. | I0 extracts the registered branch as `sendRegistered` (pure move) so the fan-out and the walk call it per member. Not an ADR change. |
| D6 | Rule 8: the outbound record "holds the responder leg only"; copy and passed legs are "their own record keyed by (`message_id`, recipient)". | The outbound key is `s\x00message_id`; `askedPeer` and `rearmDeadlines` read it. | New journal type `leg` keyed `(message_id, agent_id)`; the walk's candidate rewrite is an ordinary outbound append (latest wins). `liveSet` retention rule for `leg` is part of I6. Matches ADR. |
| D7 | Rule 2: `inbox:` resolves only to an inbox; `inbox:<name>@<host>` is `rejected/remote_inbox_unsupported`. | `SplitAddress` runs inside `ResolveTarget`. | I5 checks the `inbox:` prefix before `SplitAddress`, so `inbox:x@host` never reaches the paired-alias logic. |
| D8 | Rule 1: `claim("inbox:<name>")`, `release` has no inbox form. | `claimTarget` calls `ResolveTarget` first. | I3 intercepts the prefix before `claimTarget`. |
| D9 | Consequences: "ten tools". | Seven tools today. The #43 plan adds an eighth. | Count text is updated in the commit that adds each tool. See Open questions Q4. |
| D10 | Offline-spool sketch: "decide it together with ADR 0023's deferred pre-bind inbox". | The pre-bind store is now ruled (ADR 0026); the spool is not. | Edit ADR 0024's follow-on paragraph in place: the pre-bind store is built by ADR 0026, the spool stays deferred and reuses its storage technique. |
| D11 | ADR 0023 names the store "pre-bind inbox", directory `inbox/`. | Collides with role inboxes (`inboxes/`). | ADR 0026 renames it "pre-bind store", directory `prebind/`. ADR 0023 gets a status note. |

Everything else in ADR 0024 was checked against the files it names (`claims.go`, `hints.go`, `target.go`, `discovery.go`, `receiver.go`, `journal.go`, `registry.go`, `a2a/tasks.go`) and the named symbols exist. The design does not need to change; ADR 0024 gets only the in-place edits for D1 and D10 (and the tool count when built).

## 4. Pre-bind store (#41) and the receiver chain

The chain is authenticate → dedupe → hop-chain → policy → inject, and nothing injects from an ingress handler. The store does not break that:

- **Write (sender side).** After a native inform returns `accepted` or `queued`, `deliver` appends one MAC'd entry to `prebind/<target agent_id>.jsonl` (ADR 0026, ADR 0023 rule 1). It touches the sender's journal as today and writes no reply route.
- **Drain (receiver side).** `Shim.Bind` calls a new `drainPreBind` after `Receiver.Recover` and before the descriptor is published. It hands each verified entry to `Receiver.ImportDelivered`, which does the authenticate step (MAC, binding), the dedupe step (`AcceptRecord`, idempotent), and opens the inform route with the supplied open time. It skips hop, policy and inject because the message reached the model before the bind: the same exemption `Recover` has. The skipped steps are named in ADR 0026 rule 2 so a reviewer can object.
- **Journal and replay interaction.** An import writes the same two records a live delivery writes (a dedupe record with the sender's agent_id and the delivery state; an open route). On the next restart `Recover` rebuilds the route from `At`, so expiry is `InformRouteTTL` after the sender's delivery time. Journal retention already keeps an open inform route until it expires. A crash mid-drain is finished by the next drain (claimed file, idempotent import); see the tests.
- **Late entry.** `Shim.Reply` drains once on a miss before returning `no_such_message`.
- **Interaction with inboxes.** Inbox members are registered agents, so inbox fan-out never goes through the store. A session whose first CrossTalk call is `attach` binds first (the call registers it), then the drain has already run. Lock order: store lock, then reply lock; inbox operations take the inbox lock, then the agent's `mu`, then `Registration.mu`; no path holds the store lock and the inbox lock together.

## 5. Commit sequence

Each commit is one logical change with its tests, its doc slice (design section, AGENTS.md, skill, faq as listed), and the trailer `Provisional-ADR: 0024` or `0026`. Every commit builds and passes `go test ./...` on its own. Track A (A1 to A4) lands first because it fixes a live bug and is self-contained; Track B follows.

Notation: **Before** is the observable behaviour on main; **After** is what the new test asserts. Each test must fail on the parent commit.

### Track A: pre-bind store (ADR 0026)

**A1. `internal/prebind`: store, MAC, locks, caps (no wiring).** Files: new `internal/prebind/prebind.go`, `prebind_test.go`.
- `TestPreBindEntryRoundTrip`: Before: no package. After: an appended entry verifies with the sender's token.
- `TestPreBindDropsBadMACWrongBindingModeSymlinkNoToken`: each tamper is dropped and never returned.
- `TestPreBindCapsSkipWrite` (64 entries, 4 MiB, 256 files, 16 MiB, claimed files counted) and `TestPreBindSweepsEntriesOlderThanTTL`.
- `TestPreBindDrainCrashPoints`: crash after rename, after the claimed file is partly consumed, leaves a claimed file the next drain finishes.
- `TestPreBindCollisionDropsConflictingSenders`.

**A2. `Receiver.ImportDelivered` and route open time.** Files: `internal/receiver/receiver.go`, `receiver_test.go`.
- `TestImportDeliveredOpensRouteWithSuppliedTime`: Before: `openRoute` stamps now. After: `OpenedAt` is `min(delivered_at, now)` and survives `Recover`.
- `TestImportDeliveredIsIdempotent`: second import writes nothing; an answered or expired route is never reopened.
- `TestImportDeliveredNeverInjects`: the fake adapter sees zero injects.
- `TestImportedRouteExpiresOneHourAfterDelivery`: imported 55 minutes after delivery, expires 5 minutes later, also after restart.

**A3. Sender writes the entry; hint changes.** Files: `internal/shim/send.go` (native branch of `deliver`), `internal/shim/hint_test.go`.
- `TestNativeSendWritesPreBindEntryOnlyForAcceptedOrQueuedInform`: Before: no file. After: a file for `accepted`/`queued` inform to a native session; none for `unknown`, `rejected`, a registered target, or an ask.
- `TestFailedPreBindWriteKeepsStateAndAddsHint`: state unchanged, old one-way hint kept.
- `TestAcceptedNativeSendWarnsOneWay` updated: with a stored entry the hint says the recipient can answer with `reply` after it uses CrossTalk.

**A4. Drain at bind and on a `reply` miss or an unfinished import.** Files: `internal/shim/agent.go`, `internal/shim/send.go` (`Reply`), `internal/shim/reply_test.go`, `drain_test.go`.
- `TestPreBindReplySucceedsAfterBind` (the end-to-end test of #41): a registered sender informs a native session; the session binds; `reply` with the delivered message id returns `accepted`. Before: `invalid/no_such_message`. After: delivered to the sender, route closed, a second `reply` with other text is `already_replied`.
- `TestPreBindDrainRunsBeforePublish`; `TestPreBindReplyDrainsLateEntry` (an entry written after the bind is found by the miss path); `TestPreBindPartialImportRepairedOnRetry` (a one-time route-append failure during a late drain leaves the dedupe record; the same `reply` retried without a restart drains again, completes the route and succeeds; a closed route is never reopened); `TestPreBindEntryOlderThanTTLGivesPreBindHint`.
- Docs: design §5 and §12 S5, `docs/faq.md`, skill text; ADR 0023 status note; ADR 0026 Trial record.

### Track B: role inboxes (ADR 0024)

**I0. Extract `sendRegistered` from `deliver`.** Pure move of the registered branch (journal outbound, `viaTransport`, hints, `countSend`). Files: `internal/shim/send.go`. No new test; the existing `send`/`hint`/`receipt` tests are the pin. Reviewer checks the diff is a move.

**I1. Inbox store and ownership state machine.** Files: new `internal/registry/inbox.go`, `inbox_test.go`; `descriptor.go` gains `Inboxes []InboxMembership` (`json:"inboxes,omitempty"`). Operations (create-on-attach, grant, hand over, detach, claim) run entirely under `inboxes/<name>.lock` and write `inboxes/<name>.json` atomically.
- `TestAttachCreatesInboxOwnedByCaller`, `TestAttachOwnedInboxNeedsGrant`, `TestGrantOnlyByOwner`, `TestGrantOwnerHandsOverInOneWrite`, `TestOwnerDetachLeavesInboxOwnerlessAndKeepsGrants`, `TestClaimOwnerlessInbox` (and `inbox_owned`), `TestAttachOwnerlessRejectedWithClaimHint`, `TestClaimerInheritsGrants`.
- `TestInboxConcurrentClaimAndResume`: two goroutines, one winner.
- `TestInboxCrashBetweenFileAndDescriptor`: crash points injected between file write, journal append and publish; the inbox is ownerless and a displaced owner does not regain authority.
- `TestDescriptorWithInboxesReadByOldShape`: an old-shape struct decodes the new JSON.
- `TestListSkipsSupersededMemberAndDeadMember`: `InboxMembers` returns only live, non-superseded descriptors.
Before: no inbox concept. After: the state machine is correct without any tool or network.

**I2. Journal record for membership.** Files: `internal/journal/journal.go` (`TypeAttach`, key by inbox name, `unresolved()` keeps the latest while attached), `journal_test.go`.
- `TestAttachRecordSurvivesRetention`: a 25-hour-old attach record is kept; a detach record lets it age out. Before: unknown type, dropped at retention.

**I2b. Receive-side copy support (before any member can exist).** Files: `internal/a2a` (map `metadata.crosstalk.inbox`/`inbox_copy` to `Inbound`; skip `MaxOpenAsks` for a copy; complete an ask copy on delivery), `internal/receiver` (authorize the copy against its original kind; journal dedupe and task with the marker; open no route), `internal/journal` (`Inbox`, `Copy`, `OrigKind` fields), `internal/shim` `Reply` (`rejected/inbox_copy`). Nothing sends a copy yet.
- `TestInboxCopyOpensNoRoute` (and `reply` to it is `inbox_copy`), `TestInboxForgedCopyFlagOnlyRemovesRoute`, `TestInboxCopyAuthorizedAgainstOriginalKind`, `TestInboxAskCopiesNeverFillOpenAsks`.
- `TestPendingCopyPersistedAsInformForOlderReaders`: a copy submitted but not yet dispatched is stored with kind `inform` and `OrigKind` set, so a reader that ignores the markers (rollback, see section 6) resumes an inform, never an ask.
Ordering rule: no commit that lets a session attach (I3 and later) precedes this one, so every build that can publish a membership can receive a copy. Mixed-build rule: because a member is created only by `attach`, and `attach` exists only from I3, an I7 sender never selects a member whose receiver predates I2b.

**I3. The three tools and the `inbox:` form of `claim`/`release`.** Files: `internal/mcpserver/{server.go,claims.go,backend.go,hints.go,fake.go}`, `internal/shim/` handlers beside `claims.go`, new `Agent.applyAttach`/`applyDetach` (same pattern as `applyClaim`: admit, `a.mu`, journal, republish).
- `TestInboxToolsRefuseSubagents` (`caller_not_primary` for all four), `TestInboxActionsSelfOnlyAndUnbound` (`unbound`, and no peer can act for a session), `TestClaimInboxFormDoesNotResolveASession`, `TestReleaseInboxFormIsInvalidInbox`, `TestInvalidInboxNames` (empty, `@`, `/`, control characters).
- `TestToolListHasAttachDetachGrant`: the registered tool set. Before: seven; After: ten. Also confirms the install-time allow rule (`internal/harness/claude/install.go`) and Codex `default_tools_approval_mode = "approve"` cover new tools without a prompt (the one thing to verify by hand on a clean account).
- Docs: design §7, §8; AGENTS.md tool count; MCP `Instructions`; skill (attach, grant and claim only for an inbox the user or the brief names).

**I4. Resume replay in `register()`.** Files: `internal/mcpserver/server.go`, `internal/registry/inbox.go`.
- `TestInboxAttachSurvivesRestartAndKeepsOrder` (original `attached_at`, a record older than the retention window, an owner that resumes with grants intact, a member not in `grants` whose owner changed is dropped with a `notice`).
- `TestInboxSupersededMemberDropsOut` (Claude `/clear`).
Before: a restarted shim lists no inboxes. After: it republishes them with the original order.

**I5. `inbox:` address resolution and `list_agents`.** Files: `internal/mcpserver/target.go`, `discovery.go`, `resolve.go`, `golden_test.go`.
- `TestInboxAddressNeverMatchesSessionName` (a session named `inbox:x` is reachable only by agent_id), `TestInboxRemoteAddressRejected` (`inbox:x@host` is `rejected/remote_inbox_unsupported`, no network call), `TestInboxZeroMembersNoSuchAgent` (with the inbox-specific hint).
- `TestListAgentsShowsInboxOwnerAndResponder`; `TestLocalListAgentsRowsUnchanged` (existing golden) must still pass byte for byte when no inbox is attached.

**I6. Send to an inbox: one responder, no copies yet.** Files: `internal/shim/send.go`, `internal/mcpserver/backend.go` (`Result` gains `inbox`, `responder`), `internal/journal` (`leg` type and its retention).
- `TestInboxNewestMemberResponds`, `TestInboxAskOneReply` (the ask's task, deadline, route and outbound record are keyed as for a direct ask to the responder), `TestInboxCommandOnlyToResponder`, `TestInboxCodexThreadsIndependent`, nudge counts the inbox as one peer.
Before: `inbox:x` is `no_such_agent`. After: a member answers.

**I7. Sender fans out copies.** Files: `internal/shim/send.go`, `transport.go` and the A2A client (carry the `inbox`/`inbox_copy` metadata). The receive side is already in I2b.
- `TestInboxFanOutResponderGetsKindOthersGetCopies`, `TestInboxCopyLegNeverOverwritesResponderLeg` (also after a sender restart), `TestInboxMixedBuildReceiverNeverGetsReplyableCopy` (a receiver double without I2b semantics is never chosen: membership is the only source of recipients and the test attaches only through the I3+ path).

**I8. The fallback walk and durable candidate.** Files: `internal/shim/send.go` (walk, `CancelTask` on a pending leg), `internal/journal` (leg records, restart resume).
- `TestInboxFallbackPastUnreachableResponder`, `TestInboxFallbackAfterCanceledPendingLeg`, `TestInboxNoFallbackAfterAmbiguousDelivery`, `TestInboxCandidateRepliesBeforeSendReturns`, `TestInboxSenderCrashAfterCandidateAccepted`, `TestInboxAllMembersUnreachable` (`no_reachable_member`), `TestInboxWalkStopsOnJournalUnknown` (relies on the shipped #63 behaviour).
This is the riskiest commit: it must hold that at most one member ever holds a route.

**I9. Membership notices and handover.** Files: shim handlers from I3.
- `TestInboxMembershipNotices`; `TestInboxHandover` (the seven warm steps of #54, an ask held by the predecessor answered by it after the successor attached); `TestInboxColdHandoverClaim`.
This is the end-to-end test for #54: before, a send to the old generation-numbered name is `no_such_agent`; after, a send to `inbox:<role>` reaches a member at every step.

**I10. Docs close-out.** `docs/design.md` §3, §4, §7, §8 (ten tools), §11 (new tests), `docs/faq.md`, the skill, `docs/index.md` / `how-it-works.md` / `contributing.md` tool counts, `docs/decisions/README.md`, AGENTS.md back-links to ADRs 0024 and 0026, ADR 0024 edits for D1 and D10 and its Trial record. Keep per-commit doc slices small; this commit only sweeps what is left.

## 6. Migration and compatibility

- **Descriptors:** `inboxes` is additive and omitted when empty; `Protocol` stays 1. An older shim ignores the field. An older sender cannot address an inbox.
- **Journals and rollback:** `attach` and `leg` are new record types; an older binary reading a newer journal keys an unknown type as `?` and does not retain it, so downgrading loses membership and leg history. Pending copies are the other persisted state: they are stored with kind `inform` (I2b), so a downgraded reader resumes an inform, not an ask, and the responder's ask is never answered twice. Rollback barrier: before downgrading, detach inboxes (or accept that membership is lost); copies complete on delivery, so no pending copy outlives a quiet restart.
- **New directories:** `inboxes/` and `prebind/`, created 0700 on first use. `crosstalk uninstall` removal behaviour for state directories is unchanged (it keeps state); nothing here changes that.
- **Mixed versions during an upgrade:** fan-out needs a new shim at every member; I2b before I3 guarantees any build that can hold a membership can receive a copy. The pre-bind store needs a new shim at the sender only; an old receiver ignores the file, and `reply` stays `no_such_message` with the existing hint.
- **Tool surface:** seven to ten tools. The skill and server instructions change in the same commit as the tool.

## 7. Risks

1. **Hot-path change in `deliver`.** Mitigation: I0 is a pure move first; I6 to I8 add one branch each.
2. **Double reply or lost reply in the walk.** Mitigation: the walk only moves past a member with journaled evidence of non-delivery; an ambiguous leg stays the responder (ADR 0024 rule 5). `TestInboxNoFallbackAfterAmbiguousDelivery` and `TestInboxSenderCrashAfterCandidateAccepted` pin it.
3. **Lock ordering.** Inbox lock, then agent `mu`, then `Registration.mu`; the pre-bind lock is never held with the inbox lock. A race test over attach/claim/resume (I1) runs under `-race`.
4. **Token cost of copies.** A message to an inbox with k members is injected in k sessions (accepted in ADR 0024).
5. **Pre-bind forgery surface.** A same-user process holding an agent's token can open an inform route in a receiver's journal. Accepted in ADR 0023 and kept; the text never reaches the model.
6. **Typo creates an inbox.** `attach("coordintor")` silently creates a new inbox and its owner. See Q2.
7. **Size.** About fifteen commits and four ADR/doc files; the series is stacked so each can merge on green CI.

## 8. Open questions for the operator

- **Q1. One series or two releases?** The fallback walk (I8) is the largest and riskiest part and is needed only when a responder dies mid-send. Recommendation: land A1 to A4 and I0 to I7, I9, I10 first (handover works because the newest member answers), and I8 right after, in the same release if its tests are green, else the next. This is a sequencing choice the rulings do not cover.
- **Q2. Show `created: true` in the `attach` result, and a hint when it created a new inbox?** ADR 0024 does not say. It makes a typo visible to the model. Recommendation: yes; it adds one result field and no behaviour.
- **Q3. Mixed-version note.** Is it acceptable that an inbox send to a member on an older shim is impossible (members are new shims by construction), so no downgrade path exists for inbox users? Recommendation: yes.
- **Q4. Overlap with the #43 rename tool.** The #43 plan adds an eighth tool; this series takes the count to ten (attach, detach, grant), eleven if #43 also lands. ADR 0024 rejected a `rename` tool, so the two decisions collide. The tool-count text (design §8, AGENTS.md, skill, instructions, five doc pages) is edited by both series. Recommendation: land one series first and rebase the other, or settle the #43 decision before I3.
- **Q5. Pre-bind MAC design size.** ADR 0026 keeps ADR 0023's full design (MAC, lock, caps), which ADR 0023 itself called heavy for a rare case. The operator ruled the store is built; if a smaller version is wanted (for example a journal-pull on bind), say so before A1. Recommendation: keep the reviewed design.

## 9. Definition of done for the series

- Every commit above merged with its tests; `go test -race ./...` and CI green on Linux and macOS.
- `TestPreBindReplySucceedsAfterBind` and `TestInboxHandover` pass, and each fails on `main`.
- `crosstalk install --check` still passes with the three new tools.
- ADRs 0024 and 0026 have their Trial record updated; design §3, §4, §7, §8, §11 and the skill match the code.
- #54 and #41 closed by the final PR.

## 10. Codex review

One adversarial pass (`codex-companion adversarial-review --base main`, verdict needs-attention). Three findings; each checked against the source, none auto-applied.

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | high | Membership (I3) becomes visible before receivers understand copies (I7), so a later-commit sender could pick an I3 to I6 member that treats the copy as an ordinary ask and opens a second reply route. | **Fixed.** Receive-side copy handling moved into new commit I2b, before any commit that lets a session attach; I7 is sender-only. Added `TestInboxMixedBuildReceiverNeverGetsReplyableCopy` and the ordering rule in I2b. The claim "a member is a new shim" is now backed by ordering, not by assertion. |
| 2 | high | Rollback: a copy persisted as a task of kind `ask` with new markers is resumed by an older reader as an ordinary ask, opening a route next to the responder's. | **Fixed.** Copies are persisted with kind `inform` and an `OrigKind` marker, after the authorization against the original kind (I2b, `TestPendingCopyPersistedAsInformForOlderReaders`); section 6 states the rollback barrier. Residual, accepted: an older reader resumes an inform route for the copy, so a watcher could send a plain inform to the sender; it closes no ask and matches no receipt (`askedPeer` compares the replier with the asked responder). The persisted kind differs from the kind ADR 0024 rule 6 says the copy "keeps"; the displayed kind stays the original, so this is an implementation detail and ADR 0024 gets a one-line in-place note in I10. |
| 3 | medium | A partial late import (dedupe appended, route append failed) is never repaired because the miss-only drain does not run once the dedupe record exists. | **Fixed.** `Reply` also drains once when the message is found, is an imported inform, and its route is not open and not closed in the journal (ADR 0026 rule 3 updated). Added `TestPreBindPartialImportRepairedOnRetry` in A4. |

Not checked by the reviewer (stated by it): runtime tests; this was a documentation review against current source.
