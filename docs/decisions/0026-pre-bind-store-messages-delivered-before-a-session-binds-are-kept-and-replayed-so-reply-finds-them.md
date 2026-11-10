# ADR 0026 — Pre-bind store: messages delivered before a session binds are kept and replayed so reply finds them

- **Status:** Provisional (operator 2026-11-09)
- **Date:** 2026-11-09
- **Deciders:** operator + Claude
- **Context source:** issue #41 (dogfood, seen four times); operator ruling (2026-11-09): fold the pre-bind store into the role-inbox build ([ADR 0024](0024-role-inboxes-beside-session-names.md), plan `docs/plans/role-inboxes-54.md`); [ADR 0023](0023-replies-to-messages-delivered-before-bind-the-envelope-carries-the-sender-agent-id-inbox-deferred.md) (the middle path, built, and the design this ADR builds)

## Context

A session with no CrossTalk binding can still receive an `inform` injected by the sender's shim (the `tgt.Native` branch of `deliver`, `internal/shim/send.go`). The receiver's journal gets no dedupe record and no reply route for it, so after the session binds, `reply` on that message id returns `no_such_message` (`Shim.Reply`, `internal/shim/send.go`). ADR 0023 shipped the middle path (the sender's agent_id in the envelope, the pre-bind hint) and deferred the store that would make `reply` work. The operator has now ruled that the store is built, so a first reply from a fresh session stops costing a failed call.

ADR 0023 called the store a "pre-bind inbox". ADR 0024 introduced role inboxes (`inbox:<name>`). The two are unrelated, so this ADR calls the store the **pre-bind store** and uses the directory `~/.local/state/crosstalk/prebind/`; the role-inbox directory is `inboxes/`.

## Decision

Build the design ADR 0023 recorded under "Deferred: pre-bind inbox" (its Design rules 1 to 7 and its tests), unchanged except for the points below. ADR 0023's text is the specification; this ADR records only the differences.

1. **Name and path.** "Pre-bind store", `~/.local/state/crosstalk/prebind/<target agent_id>.jsonl` (claimed file `<agent_id>.claimed`, lock `prebind/.lock`), replacing `inbox/` in ADR 0023 rules 3 and 6.
2. **Drain runs in registration, not in an ingress.** The shim drains the store in `Shim.Bind` after `Receiver.Recover` and before the descriptor is published (`internal/shim/agent.go`), under the agent lock already held. The import is a new receiver method, `ImportDelivered`, the only writer of imported records. It runs the chain's authenticate step (MAC, binding, mode and owner checks) and its dedupe step (`Journal.AcceptRecord`, so an import is idempotent per sender and message id), then opens the inform route with the supplied open time. It skips hop-chain and policy and never injects, because the message already reached the model: it is journal-only recovery, like `Receiver.Recover`. No ingress handler injects, so the canonical-pipeline rule in AGENTS.md holds.
3. **Late entries.** `Shim.Reply` drains once more on a miss before it returns `no_such_message`, as ADR 0023 rule 4 says. The drain takes the store lock and the agent's reply lock in that order and nothing else, so it cannot deadlock with inbox operations (ADR 0024), which take the inbox lock first and never the store lock.
4. **Route open time.** `openRoute` stamps `r.now()` today (`internal/receiver/receiver.go`). `ImportDelivered` uses a variant that journals the supplied time `min(delivered_at, now)`; `Recover` already restores `OpenedAt` from the journaled `At`, so a restart keeps the original expiry.
5. **Hints.** The sender's one-way hint (`hintNativeOneWay`) changes only when the store write succeeded: it says the recipient can answer with `reply` once it has used CrossTalk, and the old text stays for a failed or capped write. `hintNoRoutePreBind` stays for every case the store does not cover.
6. **Scope.** The store holds informs to native sessions that were `accepted` or `queued`. Offline delivery (a message to a session that is not running) stays the deferred "offline spool" of ADR 0024; this store never injects.

The rest of the mechanism is ADR 0023's, kept as written: MAC under the sender's own agent token (`tokens/<sender agent_id>.token`, `internal/wire/wire.go`), no-follow opens, mode 0600 in a 0700 directory, one advisory lock, per-target and directory caps, `InformRouteTTL` expiry from the delivery time, the collision rule (entries with one message id and different senders are dropped), and remote senders excluded.

## Rejected alternatives

- **Leave #41 on the ADR 0023 middle path.** Rejected by the operator (2026-11-09); the hint workaround cost a failed call each time a fresh session answered its first message.
- **Bind the receiver on delivery.** Still the cross-agent identity claim ADR 0004 forbids (ADR 0023, Rejected alternatives).
- **The receiver pulls from the sender's journal on bind.** Rejected in ADR 0023 for the same reasons: it scans every journal and reads another agent's private record format.
- **A smaller store with no MAC.** A same-user process could open a route naming any registered agent as the sender, which is sender identity from message content (ADR 0004). The MAC keeps the forger to an agent whose token it can read.
- **Call it the "pre-bind inbox".** The name collides with the role inboxes of ADR 0024.

## Consequences

- **Trust.** Unchanged from ADR 0023 Consequences: a forged entry needs a readable agent token, never reaches the model's text, and lives inside the same-user non-goal (design §1).
- **Design and docs when built:** §5 (journal gains the store as a second source of dedupe and route records on registration), §12 S5 (`reply` to a message delivered before bind), `docs/faq.md` and the skill text that says such a message cannot be answered with `reply`.
- **ADR 0023** gets a status note that its deferred design is built by this ADR; its middle path stays in force.
- **Tests that gate this ADR:** those listed in ADR 0023 Consequences, plus `TestPreBindDrainRunsBeforePublish`, `TestPreBindReplyDrainsLateEntry` and `TestNativeSendWritesPreBindEntryOnlyForAcceptedOrQueuedInform`.
- Implementation commits carry `Provisional-ADR: 0026`.

## Trial record

- Council verdict: none; the operator ruled on 2026-11-09 that the store is built.
- Rests on: ADRs 0004, 0006, 0009, 0010, 0023 (provisional).
- Commits: `git log --grep 'Provisional-ADR: 0026'` (none yet).
- Pinning test: the tests listed in Consequences.
- Outcome: draft, not built.
