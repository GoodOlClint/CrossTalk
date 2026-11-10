# ADR 0027 — Rename tool lets a session set its own name

- **Status:** Provisional (operator 2026-11-09, ruling on #43; open questions decided 2026-11-10; built)
- **Date:** 2026-11-09
- **Deciders:** operator + Claude + Codex review
- **Context source:** issue #43 and its comments; operator ruling 2026-11-09 on #43 (a rename tool, not an environment variable, not docs only); plan `docs/plans/rename-tool-43.md`; operator rulings 2026-11-10 (plan section 7)

## Context

A CrossTalk name is a per-host alias for an `agent_id`, held under its own alias lock for the registration's lifetime (design §3, ADR 0003). The shim takes it from the harness and `followRename` (`internal/mcpserver/server.go`) moves the alias when the harness's name changes. Claude gets a launcher-set name from `claude -n`. Codex has no launch-name flag, and its name is the auto title of the thread, so a launcher that names a Codex session in its prompt cannot find it by that name (#43). Identity is per request and one Codex process may serve several threads, so a process-scoped setting cannot name one thread (ADR 0006).

ADR 0024 rejected a `rename` tool in its first draft (operator, 2026-11-07) in favour of role inboxes, which address a role that several sessions hold. On 2026-11-09 the operator ruled for #43 that a rename tool is built, not an environment variable and not docs only. An inbox names a role; a rename names one session. They answer different needs, and #43 is the second.

A second observation on #43 is that a Codex row before bind and after bind carried different `agent_id`s. That is a different `session_ref`, so a different thread; a rename cannot affect it and this ADR decides nothing about it.

## Decision

Add an MCP tool, `rename(name)` → `{name, previous}` (`internal/mcpserver/rename.go`).

1. **Self only.** It acts on the calling agent, from request identity (ADR 0004, 0006). There is no target argument. Any bound caller may use it, primary or subagent; a name is an alias and grants no authority. An unbound call gets `unbound`.
2. **Uniqueness is the alias lock, unchanged.** The new alias lock is taken first, then the descriptor is rewritten, then the old lock is released (`Registration.Rename`). A name held by another live registration is `rejected/name_held`; the caller keeps its alias and no fallback name is made up. `agent_id` and every path keyed on it do not change.
3. **Validation** (`identity.ValidName`). A name is 1 to 64 bytes, has no control character and no `@`, is not 16 lowercase hex characters (it could never resolve), and does not start with `inbox:` (reserved for ADR 0024). Spaces are allowed. Failure is `rejected/invalid_name`. `rename` to the current alias succeeds and pins it.
4. **A rename pins.** While a name is pinned, `followRename` (through `followPin`) tracks the harness's name but moves the alias only to the pinned name: the pin wins over Claude `-n`, a user's Claude `/rename` (operator ruling) and Codex's auto title, and a pinned session whose alias differs from its pin (after a restart that found the name held) retries the pin on every call. There is no unpin; the session renames again.
5. **Persistence** (operator ruling): the pin is a journal record (`TypeName`), retained while set and replayed at registration before the first publish. If the pinned name is held then, the session registers under its fallback with a notice and the retry in rule 4 takes it once free. A journal rewritten by a shim without this record type drops the pin; the downgrade policy is in the plan (section 8), pinned by `TestNameRecordSurvivesOldShimRewrite`.
8. **A released registration refuses `Rename`** (`registry.Registration.Rename` returns `ErrClosed` after `ReleaseName`), so an in-flight rename cannot republish the descriptor of a retiring agent.
6. **Local only.** The gateway serves no `rename` (ADR 0022); `@` is excluded so `name@host` stays unambiguous.
7. **Codex launch pattern.** The brief gives the session a name and tells it to call `rename` as its first CrossTalk call; the launcher then resolves it by name. A pre-bind `agent_id` is still not stable and is not addressed here.

This reverses the `rename` rejection in ADR 0024 (its Rejected alternatives entry, its Context paragraph on the first draft, and its #43 paragraph), and narrows its statement that session names and `followRename` do not change: only an unpinned session keeps today's `followRename` behaviour. ADR 0024 is amended in place to match.

## Rejected alternatives

- **A launch environment variable (`CROSSTALK_NAME`).** Process-scoped; one Codex process may serve several threads and the managed app-server, not the launcher, starts it. Rejected by the operator on #43.
- **Docs only (address by `agent_id`).** Does not give the launcher a handle before it has the id. Rejected by the operator on #43.
- **A name argument on an existing tool.** Tool results should not change identity as a side effect; a tool whose only job is the rename is clearer to a model and to the skill.
- **A rename that overrides harness names only until the harness changes its name.** For Codex the auto title changes after the first turn, so the pin would be lost at once.
- **A user `/rename` clears the pin.** Needs a per-harness capability, because Codex's title change looks the same at the shim; ADR 0008 keeps harness branching out of the shim. Rejected by the operator on 2026-11-10.
- **Role names through `rename` (a successor takes the predecessor's name).** The alias lock allows one holder, so there is no overlap at a handover; that is the case ADR 0024's inbox serves.

## Consequences

- **Trust (ADR 0004).** A name carries no authority. Any bound same-user session can take a free name, including one a peer expects; this is the existing exposure of taking a free harness or fallback name, inside the same-user non-goal (design §1). The skill already says to resolve a peer by `list_agents` before the first send.
- **Shadowing.** The alias lock does not see sessions with no CrossTalk binding, and a registered name resolves first, so a rename to a visible native session's name hides that session by name. Not refused in v1 (operator ruling, 2026-11-10); revisit if dogfood shows it.
- **Stale peers** get `no_such_agent` and the existing hint, as for a harness rename today.
- **Design changes:** §3 (the pin and the name record), §8 (the `rename` entry), §11 (test 42); `AGENTS.md`; the skill; `docs/how-it-works.md`, `docs/index.md`, `docs/contributing.md`; the hints for `invalid_name` and `name_held`.
- **Files:** `internal/identity` (validation), `internal/journal/journal.go` (the record), `internal/mcpserver/` (`server.go` pin rule and replay, `rename.go`, `hints.go`), `internal/registry/registry.go` (a released registration refuses `Rename`).
- **Tests that pin it:** `TestValidName`, `TestNameRecordRetained`, `TestRenameChangesAliasKeepsAgentID`, `TestRenameNameHeldRejected`, `TestRenameInvalidName`, `TestRenameSameNameStillPins`, `TestFollowRenameIgnoresHarnessNameWhilePinned`, `TestUnpinnedFollowRenameUnchanged`, `TestRenameCodexThreadsIndependent`, `TestRenamePinSurvivesRestart`, `TestRenamePinHeldOnRestartRetriesWhenFree`, `TestRenameJournalFailureRollsBack`, `TestNameRecordSurvivesOldShimRewrite`, `TestRenameAfterReleaseNameRefused`.
- **Overlap with ADR 0024 (#54).** Both add tools. By operator ruling the docs list the tools and never count them, so the second build adds names without a count to fix.

## Decided open questions

All four were ruled on by the operator on 2026-11-10 (plan section 7): the pin persists across restart; a user `/rename` on Claude does not clear it; a name held by a visible native session is not refused in v1; the cap is 64 bytes and `inbox:` is reserved. The docs list the tools instead of counting them.

## Trial record

- Council verdict: none; the operator reviewed the plan and ruled on its open questions before the build.
- Rests on: ADRs 0003, 0004, 0006, 0008, 0022 and 0024 (provisional).
- Commits: `git log --grep 'Provisional-ADR: 0027'` .
- Pinning test: the tests listed in Consequences.
- Outcome: built on branch plan-43-rename (#43); live end-to-end Codex run not yet recorded.
