# ADR 0024 — Agent naming: a session sets its own name with a rename tool, and the name stays until it changes it

- **Status:** Provisional (operator 2026-11-07)
- **Date:** 2026-11-07
- **Deciders:** operator + Claude (Opus 5.5) + Codex review
- **Context source:** issues #43 and #54 (dogfood); triage of both; operator ruling 2026-11-07 on that triage: docs now (PR #56), design later

## Context

A CrossTalk name is a per-host lookup alias for an `agent_id`, held under its own alias lock for the registration's lifetime (design §3, ADR 0003). The shim takes it from the harness: the Claude session name, or the Codex thread name. If that name is taken, the agent registers under the fallback `<harness>-<cwd basename>-<hex>`, and on every bound call `followRename` (`internal/mcpserver/server.go`) moves the alias back to the harness name once it is free, through `Registration.Rename` (`internal/registry/registry.go`). Storage, sockets, journals, claims and reply routes key on `agent_id`, the fixed hash of (harness, session_ref); the name only resolves `to` at send time (`Registry.Lookup`). Nothing in CrossTalk sets a name except the harness.

**#43, Codex has no launcher-set name.** Claude gets a name from `claude -n`. Codex has no launch-name flag (upstream). Codex `Identify` returns only the thread id from `params._meta.threadId` (`internal/harness/codex/harness.go`), so `Server.resolve` copies the name from Discover, which is the `thread/read` name (`internal/harness/codex/discovery.go`): Codex's auto title for the thread. A launcher that named the session in its prompt cannot find it by that name and must use `agent_id`. Identity is per request and one Codex process may serve several threads, so anything process-scoped cannot name one thread.

A second observation on #43 (dogfood, 2026-11-07): before a Codex session bound, `list_agents` showed a nameless row with one `agent_id`; after it bound, the row showed the auto title with a different `agent_id`, so a peer that saved the pre-bind id lost the address. The native row and the bound row compute `agent_id` the same way (`identity.AgentID(harness, session_ref)` in `listAgents`, `internal/mcpserver/discovery.go`, and in `Server.agent`), so the two rows had different session_refs. The triage suggests the pre-bind row was another thread that never ran a turn; that is not confirmed. This ADR does not decide anything about it.

**#54, a saved name breaks at a successor.** A long-lived role (a coordinator) is replaced by a fresh session whose name carries a generation number. Peers that saved the old name get `unknown` / `no_such_agent`; the hint (`internal/mcpserver/hints.go`) sends them to `list_agents`, and nothing points at the successor. This works as designed, but every sender has to work out who replaced whom.

**Already shipped (PR #56, docs only):** briefs use a role name, and a sender runs `list_agents` before its first send. That is option (a) below. It cannot fix #43: a Codex session has no way to hold a role name, so a brief cannot name it.

## Decision

Add one MCP tool, `rename(name)`, through which a bound session sets its own CrossTalk name. The name stays until the session changes it. Nothing else is added: no role alias, no successor record, no new descriptor field.

1. **Who.** `rename` has no target argument. It renames the calling agent, whose identity comes from the request (Claude parent pid, Codex `_meta.threadId`; ADR 0004, ADR 0006). An `unbound` call gets `unbound`, as for every tool except `list_agents`. No message, envelope or peer can set or change a name.
2. **Uniqueness.** The name is taken under the existing alias lock through `Registration.Rename`. A name held by any live registration, superseded or not, is `rejected/name_held` and the caller keeps its current name. A held name is never taken from its holder.
3. **Validation.** `rename` refuses, as `rejected/invalid_name`: a name of 16 lowercase hex (the `agent_id` shape; design §8 would make it unaddressable by name); a name containing `@` (under ADR 0022 an exact local match wins over `name@host`, so a local `x@desk` would capture local sends meant for the agent `x` on the paired host `desk`); a name with control characters. Harness names are not newly restricted.
4. **Stays.** A successful `rename` pins the name: `followRename` stops following the harness name for that agent, so a later Codex title change or Claude `/rename` does not move it. The pin is journaled in the agent's own journal and replayed on registration, like a claim, so it survives a shim restart and a resume of the same session_ref. On replay the pinned name takes the place of the harness name in the §3 rule: held → fallback name with the existing notice, retried on each bound call.
5. **Return.** `rename("")` clears the pin; the agent goes back to following the harness name under the §3 rule.
6. **Result.** `{name}` on success. On `name_held` the hint says the name is held by a live session and names `list_agents`; it never names the holder's `agent_id` beyond what `list_agents` already shows.

How it answers each issue:
- **#43.** A Codex session renames itself to the name its launcher chose, and peers address that name. Each pooled thread renames only its own agent record, because each call carries its own `threadId`; a thread cannot rename a sibling thread in the same process. The Codex thread title in Codex's own UI is unchanged: CrossTalk reads harness registries and never writes them (design §3).
- **#54.** A role is a name, handed over with `rename`. Cold handover: the predecessor is gone, its alias lock is released, and the successor calls `rename("<role name>")`. Warm handover: the predecessor calls `rename("")` or renames itself to another name, which frees the role name, then the successor takes it. Peers that saved the role name reach whoever holds it. Between the release and the take, a send by name is `no_such_agent`, as today.

## Rejected alternatives

- **(a) Docs only (PR #56, the status quo).** Kept as the baseline guidance, not as the decision: it gives a Codex session no way to hold a name the launcher chose, so #43 stays open, and for #54 it asks every launcher to use names the harness may not give them.
- **A name argument on an existing tool.** `list_agents` is the only tool an unbound call may use and is read-only; a write on it would mix the two. `send_message` and `ask` act on peers, not on the caller. A separate tool keeps the surface explicit.
- **A launch environment variable (`CROSSTALK_NAME`, from #43).** It is process-scoped. One Codex process may serve several threads (design §3), and the managed app-server, not the launcher, starts it, so the variable would name every thread of the process or none of them.
- **Write the Codex thread name upstream.** CrossTalk never writes harness registries (design §3), and Codex has no launch-name flag to write through.
- **(c) A retired-name-to-successor pointer in the `no_such_agent` hint.** Someone has to write the pointer. If the successor writes it, any session can declare itself the successor of any retired name and draw its traffic: an identity claim made by assertion, not transport, against ADR 0004. If the predecessor writes it, it must outlive the predecessor's descriptor, which needs new state with its own expiry, chains of pointers and pruning, and under ADR 0022 the pointer would have to cross gateways. With `rename` the role name itself moves, so the pointer is not needed for the case #54 reports.
- **(d) A claimable role alias handed over between sessions.** A second namespace beside names: its own locks, a descriptor field, a `list_agents` column, a resolution order against names and `name@host`, and handover rules in design §3 and §7, where `claim`/`release` already mean ownership. A pinned name gives the same result with the alias lock that exists. Revisit only if a session must be addressable by a role and by a personal name at once.
- **A pin that lives only in memory.** A shim restart or a resume would fall back to the Codex auto title, which is #43 again.

## Consequences

- **Trust (ADR 0004).** Only the session itself sets its name, from transport identity. The alias lock keeps names first come, first served, and a live holder is never displaced. Squatting stays possible: a same-user session can take any free name, a retired role name included, and receive what peers send to it by name. That is the exposure that exists today, since a harness name (`claude -n`, a Codex title, Claude `/rename`) already takes any free name through `followRename`; it stays inside the same-user non-goal (design §1). What is new is that the model, not only the user, can choose the name, so peer text could ask a model to take a role name. The server instructions and the skill say to rename only on the user's or the launch brief's word; the peer-text notice already tells the model not to act on a peer's word.
- **Impersonation by reply is closed as before.** Replies, claims, open asks and journals key on `agent_id`, so a session that takes a name after a handover receives only new messages sent by name. It never receives a reply to its predecessor's ask, never inherits a claim, and is never the owner of the predecessor's subagents.
- **Remote (ADR 0022, planned for v0.2.0).** `rename` is a local MCP tool; the gateway serves no rename, and a remote sender cannot rename a local agent. Names stay unique per host. A remote sender resolves `name@host` on that host at send time, so it follows a handover on that host by name; one that saved an `agent_id` does not. Refusing `@` in a pinned name keeps a local name from shadowing a `name@host` address.
- **Claude `/clear`** starts a new session_ref and so a new `agent_id` (design §3, Superseded sessions); the pin belongs to the old agent and does not carry over. The new session renames itself again if it should hold the name.
- **Not addressed:** the pre-bind row of #43 whose `agent_id` differs from the bound row. `rename` does not change any `agent_id`. A live repro should confirm the cause; until then briefs address a Codex session by the name it renames itself to after it binds.
- **Design changes when built:** §3 Name (a pinned name takes the place of the harness name, `rename("")` returns to it, the three refused shapes), §8 (eight tools; `rename(name)` → `{name}`, `name_held`, `invalid_name`), §11 (one new test), the tool count in AGENTS.md, the skill (`plugins/crosstalk/skills/crosstalk/SKILL.md`), the MCP server instructions and `docs/faq.md`. The descriptor schema (§3) and message schema (§4) do not change.
- **Files an implementation would touch:** `internal/mcpserver/server.go` (tool registration; `followRename` honours the pin; `register` reads the pin from the journal), a handler beside `claims.go`, `internal/mcpserver/hints.go` (`name_held`, `invalid_name`), `internal/journal/journal.go` (a name record type). `internal/registry` needs no change: `Registration.Rename` already takes the new lock before releasing the old one.
- **Tests that would pin it:** `TestRenameTakesFreeName`; `TestRenameHeldNameRejected` (live and superseded holders keep the name); `TestRenameStaysAcrossHarnessRename`; `TestRenameSurvivesRestartAndResume` (and falls back with a notice when the pinned name is held at replay); `TestRenameEmptyReturnsToHarnessName`; `TestRenameRejectsAgentIDShapeAtAndControl`; `TestRenameUnbound`; `TestRenameCodexThreadsIndependent` (two threads in one process rename separately; neither can rename the other); `TestRenameHandover` (A frees the role name, B takes it, a send by name reaches B, a reply to A's ask never reaches B).

## Trial record

- Council verdict: none; the operator reviews this draft before any implementation.
- Rests on: ADRs 0003, 0004, 0006, 0022 (provisional).
- Commits: `git log --grep 'Provisional-ADR: 0024'` (none yet).
- Pinning test: the tests listed in Consequences.
- Outcome: drafted; not built.
