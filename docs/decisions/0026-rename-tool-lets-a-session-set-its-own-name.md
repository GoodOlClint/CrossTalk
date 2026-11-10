# ADR 0026 — Rename tool lets a session set its own name

- **Status:** Provisional (operator 2026-11-09, ruling on #43; plan under review)
- **Date:** 2026-11-09
- **Deciders:** operator + Claude + Codex review
- **Context source:** issue #43 and its comments; operator ruling 2026-11-09 on #43 (a rename tool, not an environment variable, not docs only); plan `docs/plans/rename-tool-43.md`

## Context

A CrossTalk name is a per-host alias for an `agent_id`, held under its own alias lock for the registration's lifetime (design §3, ADR 0003). The shim takes it from the harness and `followRename` (`internal/mcpserver/server.go`) moves the alias when the harness's name changes. Claude gets a launcher-set name from `claude -n`. Codex has no launch-name flag, and its name is the auto title of the thread, so a launcher that names a Codex session in its prompt cannot find it by that name (#43). Identity is per request and one Codex process may serve several threads, so a process-scoped setting cannot name one thread (ADR 0006).

ADR 0024 rejected a `rename` tool in its first draft (operator, 2026-11-07) in favour of role inboxes, which address a role that several sessions hold. On 2026-11-09 the operator ruled for #43 that a rename tool is built, not an environment variable and not docs only. An inbox names a role; a rename names one session. They answer different needs, and #43 is the second.

A second observation on #43 is that a Codex row before bind and after bind carried different `agent_id`s. That is a different `session_ref`, so a different thread; a rename cannot affect it and this ADR decides nothing about it.

## Decision

Add an eighth MCP tool, `rename(name)` → `{name, previous}`.

1. **Self only.** It acts on the calling agent, from request identity (ADR 0004, 0006). There is no target argument. Any bound caller may use it, primary or subagent; a name is an alias and grants no authority. An unbound call gets `unbound`.
2. **Uniqueness is the alias lock, unchanged.** The new alias lock is taken first, then the descriptor is rewritten, then the old lock is released (`Registration.Rename`). A name held by another live registration is `rejected/name_held`; the caller keeps its alias and no fallback name is made up. `agent_id` and every path keyed on it do not change.
3. **Validation.** A name is 1 to 64 bytes, has no control character and no `@`, is not 16 lowercase hex characters (it could never resolve), and does not start with `inbox:` (reserved for ADR 0024). Spaces are allowed. Failure is `rejected/invalid_name`. `rename` to the current alias succeeds and pins it.
4. **A rename pins.** While a name is pinned, `followRename` tracks the harness's name but moves the alias only to the pinned name: the pin wins over Claude `-n` and `/rename` and Codex's auto title, and a pinned session whose alias differs from its pin (after a restart that found the name held) retries the pin on every call. There is no unpin; the session renames again.
5. **Persistence** (recommended, pending the operator): the pin is a journal record, retained while set and replayed at registration before the first publish. If the pinned name is held then, the session registers under its fallback with a notice and the retry in rule 4 takes it once free. A journal rewritten by a shim without this record type drops the pin; the downgrade policy is in the plan (section 3.5).
6. **Local only.** The gateway serves no `rename` (ADR 0022); `@` is excluded so `name@host` stays unambiguous.
7. **Codex launch pattern.** The brief gives the session a name and tells it to call `rename` as its first CrossTalk call; the launcher then resolves it by name. A pre-bind `agent_id` is still not stable and is not addressed here.

This reverses the `rename` rejection in ADR 0024 (its Rejected alternatives entry, its Context paragraph on the first draft, and its #43 paragraph), and narrows its statement that session names and `followRename` do not change: only an unpinned session keeps today's `followRename` behaviour. The amendment of ADR 0024 is a separate step owned by that file's writer (plan §5, step 8) and lands before the build starts.

## Rejected alternatives

- **A launch environment variable (`CROSSTALK_NAME`).** Process-scoped; one Codex process may serve several threads and the managed app-server, not the launcher, starts it. Rejected by the operator on #43.
- **Docs only (address by `agent_id`).** Does not give the launcher a handle before it has the id. Rejected by the operator on #43.
- **A name argument on an existing tool.** Tool results should not change identity as a side effect; a tool whose only job is the rename is clearer to a model and to the skill.
- **A rename that overrides harness names only until the harness changes its name.** For Codex the auto title changes after the first turn, so the pin would be lost at once.
- **A user `/rename` clears the pin.** Needs a per-harness capability, because Codex's title change looks the same at the shim; ADR 0008 keeps harness branching out of the shim. Listed as an open question.
- **Role names through `rename` (a successor takes the predecessor's name).** The alias lock allows one holder, so there is no overlap at a handover; that is the case ADR 0024's inbox serves.

## Consequences

- **Trust (ADR 0004).** A name carries no authority. Any bound same-user session can take a free name, including one a peer expects; this is the existing exposure of taking a free harness or fallback name, inside the same-user non-goal (design §1). The skill already says to resolve a peer by `list_agents` before the first send.
- **Shadowing.** The alias lock does not see sessions with no CrossTalk binding, and a registered name resolves first, so a rename to a visible native session's name hides that session by name. Open question.
- **Stale peers** get `no_such_agent` and the existing hint, as for a harness rename today.
- **Design changes when built:** §3 (the pin and the name record), §8 (the `rename` entry and the tool count), §11 (tests); `AGENTS.md`; the skill; `docs/how-it-works.md`, `docs/index.md`, `docs/contributing.md`; the hints for `invalid_name` and `name_held`.
- **Files an implementation would touch:** `internal/identity` (validation), `internal/journal/journal.go` (the record), `internal/mcpserver/` (`server.go` pin rule and replay, a handler beside `claims.go`, `hints.go`), possibly `internal/registry/registry.go` (a retired registration refuses `Rename`).
- **Tests that would pin it:** `TestValidName`, `TestNameRecordRetained`, `TestRenameChangesAliasKeepsAgentID`, `TestRenameNameHeldRejected`, `TestRenameInvalidName`, `TestRenameSameNameStillPins`, `TestFollowRenameIgnoresHarnessNameWhilePinned`, `TestUnpinnedFollowRenameUnchanged`, `TestRenameCodexThreadsIndependent`, `TestRenamePinSurvivesRestart`, `TestRenamePinHeldOnRestartRetriesWhenFree`, `TestRenameJournalFailureRollsBack`, `TestNameRecordSurvivesOldShimRewrite`.
- **Overlap with ADR 0024 (#54).** Both add tools; the count after both is eleven. Docs should list the tools rather than count them where possible. ADR numbers may collide with the inbox build's ADRs; the later merge renumbers.

## Open questions

- Persist the pin across restart (recommended yes).
- Whether a user `/rename` on Claude clears the pin (recommended no).
- Whether to reject a name a visible native session already has (recommended: not in the first cut).
- Confirm the 64-byte cap and the `inbox:` reservation.

## Trial record

- Council verdict: none; the operator reviews the plan before any implementation.
- Rests on: ADRs 0003, 0004, 0006, 0008, 0022 and 0024 (provisional).
- Commits: `git log --grep 'Provisional-ADR: 0026'` (none yet).
- Pinning test: the tests listed in Consequences.
- Outcome: not built.
