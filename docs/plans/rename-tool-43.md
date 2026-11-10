# Plan: a `rename` tool for #43

Status: approved; operator rulings of 2026-11-10 recorded in section 7. Brownfield-gate change plan. Decision record: [ADR 0027](../decisions/0027-rename-tool-lets-a-session-set-its-own-name.md).

Terms used below. `agent_id`: the fixed 16-hex hash of (harness, session_ref) that keys all storage. Alias or name: the per-host display name that resolves `to` at send time, held under its own lock file (`names/<name>.lock`). `followRename`: the code that moves the alias when the harness's own name for the session changes (`internal/mcpserver/server.go`). Pin: a name the session set itself with `rename`, which harness-driven renames no longer override. Bind: the session's first CrossTalk call, which registers it. Pooling: one Codex process serving several threads.

## 1. Goal and ruling

Goal: a launcher (or the session itself) can give a Codex session a name that `list_agents` shows and `to` resolves, because Codex has no launch-name flag and CrossTalk otherwise shows Codex's auto thread title (#43).

Operator ruling (2026-11-09, on #43): build a `rename` tool; not an environment variable, not docs only. This overturns the first-draft rejection of `rename` in ADR 0024 (operator, 2026-11-07), so ADR 0027 records the reversal. This plan does not edit ADR 0024; the amendment is step 8 below, for the writer that owns that file.

## 2. Existing state (what the tool must fit)

- **Names are aliases.** Registration takes the agent lock, then the alias lock for the harness name, or the first free fallback `<harness>-<cwd basename>-<hex>` (`registry.Acquire`, `identity.ChooseName`). Storage, sockets, journals, claims and reply routes key on `agent_id`; the name only resolves `to` (`ResolveTarget`, `internal/mcpserver/target.go`). Design §3, ADR 0003.
- **Rename machinery already exists, harness-driven.** `Registration.Rename(newName)` takes the new alias lock first, rewrites the descriptor, then drops the old lock; `agent_id` never changes (`internal/registry/registry.go`). `followRename` calls it on every bound call when the name the harness reports (`caller.name`) differs from the last one seen (`Agent.harnessName`), and retries each call while the harness name is held.
- **Where the harness name comes from.** Claude: `~/.claude/sessions/<ppid>.json` on each request, so `claude -n` and the user-typed `/rename` flow through `followRename`. Codex: `Identify` returns only the thread id, so `Server.resolve` copies the name from `Discover`, which is the `thread/read` name, Codex's auto title (`internal/harness/codex/harness.go`, `discovery.go`). Neither `-n` nor `/rename` is model-callable; the new tool is the only name change a model can make.
- **Pooling.** Identity is per request (`_meta.threadId`), so one process may serve several Codex threads. The new tool must act on the calling agent only, never on the process.
- **Name rules today.** One live holder per name (alias lock). A name that is 16 lowercase hex is never matched as a name (`IsAgentID`). `@` in `to` splits off a paired-host alias (design §8). Lock file names are percent-escaped (`nameLockPath`), so any string is filesystem-safe, but there is no length or character validation.
- **Persistence precedent.** A claim on an agent is a journal record (`TypeClaim`, key `"c"`), kept by retention while claimed and replayed in `register` (`internal/journal/journal.go`, `server.go`). A name is not persisted today: a resumed Codex session re-derives it from the harness.
- **Surfaces that say "seven tools":** `AGENTS.md` (Architecture, model-facing), `docs/design.md` §8 and §10 text, `docs/how-it-works.md:16`, `docs/index.md:23`, `docs/contributing.md:41`, the `hints_test.go` tool list, the skill's description and "Which tool" list (`plugins/crosstalk/skills/crosstalk/SKILL.md`). `README.md` and `docs/setup.md` do not count tools.
- **Not rejected by any other ADR.** ADRs 0003, 0004 and 0006 say names are aliases and identity comes from transport; a self-rename changes an alias only and takes no authority, so none conflicts. ADR 0024's rejection is the one to reverse.

## 3. Change

### 3.1 Tool

`rename(name)` → `{name, previous}`, plus `notice` through the existing `withNotice`. A new tool, served to bound callers only (`boundTool`); an unbound call gets `unbound` like every tool but `list_agents`.

- Acts on the calling agent, from request identity (Claude parent pid, Codex `_meta.threadId`; ADR 0004, 0006). There is no target argument, so no session can rename another, and no message, envelope or peer can invoke it.
- Any bound caller may call it, primary or subagent: a name is an alias, and a claim gives the owner no say over its subagent's name.
- Result: `name` is the alias after the call, `previous` the alias before. `rename` to the current alias succeeds and still pins it (this is how a session keeps the harness name against a later auto title).

### 3.2 Validation and uniqueness

A name is accepted if it is non-empty, at most 64 bytes, free of control characters and `@`, not 16 lowercase hex (it could never resolve), has no space at either end, and does not start with `inbox:` in any letter case (reserved by ADR 0024). Otherwise `rejected/invalid_name`. Spaces inside a name are allowed, because existing session names contain them.

The alias lock is the uniqueness rule, unchanged: if another live registration holds the name, `rejected/name_held`, the session keeps its current alias and pin, and no fallback is invented. A name held by a superseded session frees within the supersede interval, as for registration, so the hint says to retry. A name held by a native session that has no CrossTalk binding is not checked; see Risks.

### 3.3 Interaction with `followRename` and harness renames

`Agent` gains `pinned string`. While `pinned != ""`, `followRename` records the harness's new name in `harnessName` and moves the alias only toward the pin, silently: the pin wins over Claude `-n`, Claude `/rename` and Codex's auto title alike. The pin has no unpin form; the session renames again to change it. This is the simple rule; the alternative (a user `/rename` on Claude clears the pin) needs per-harness branching that AGENTS.md forbids outside the adapter, and is an Open question.

A pinned session whose current alias differs from its pin (a restart found the name held, section 3.5) retries `Rename(pinned)` on every call, whatever name the harness reports, including an empty one (Codex discovery can return no name). Unpinned sessions behave exactly as today.

### 3.4 Codex pre-bind versus post-bind rows

Findings on #43 (comment of 2026-11-07): before binding, `list_agents` showed a nameless row with one `agent_id`; after binding, a titled row with a different `agent_id`. Both rows compute `agent_id` with `identity.AgentID(harness, session_ref)`, so they are different `session_ref`s (probably another thread that never ran a turn; ADR 0024 records this as unconfirmed). `rename` cannot change this and does not try to: a name needs a registration, a registration needs a call from the thread, and an `agent_id` is a function of the thread id. What the tool changes is the launcher's contract: the launcher's brief tells the session to call `rename("<name>")` as its first CrossTalk call; once that returns, the launcher resolves the session by name, and both the name and the `agent_id` it then sees are the post-bind ones. A launcher that saved a pre-bind `agent_id` is still stale; the plan states that and does not fix it. The different-row cause is filed as a separate investigation (a detour, one line in OPEN THREADS), not chased here.

### 3.5 Persistence

Recommendation: persist the pin. A new journal record type (`TypeName`, with a journal key distinct from the existing ones, chosen at build), carrying `Name` and retained while set, written after the rename succeeds. `register` replays it after `journal.Open`: it calls `Registration.Rename(pinned)` before the first descriptor publish. If the pinned name is held at that moment, the session registers under the fallback and its model gets a notice, and the pin stays so a later call retries it (the same retry `followRename` already does). Without persistence, a resumed Codex session loses its name at the first restart and the launcher's contract breaks. This is an Open question because the ruling does not cover it.

Order inside the tool: take the new alias lock and rewrite the descriptor (`Registration.Rename`), then append the journal record. If the append fails, roll back with `Rename(previous)`; if that also fails, return `failed` with a notice naming the current alias. Lock-first means a crash between the two steps leaves a live name with no pin, which the session repairs by calling `rename` again.

### 3.6 Hints and error codes

New reasons, all under `rejected`: `invalid_name`, `name_held`. Hints (`internal/mcpserver/hints.go`, every entry names a real tool):

- `invalid_name`: "That name cannot be used: 1 to 64 bytes, no @, no control characters, no space at either end, not 16 hex digits, not starting with inbox: in any case. Pick another and call rename again."
- `name_held`: "Another live session already has that name. Call list_agents to see who, pick a different name, or retry once if that session has just ended."

Success carries no hint. `unbound`, `not_ready` and `agent_held` come from the existing paths.

### 3.7 Other behaviour

- **Remote (ADRs 0021, 0022).** Local only: the gateway serves no `rename`. Paired hosts' rows show the renamed name because `list_agents` is read live. `@` is banned so `name@host` stays unambiguous.
- **Role inboxes (#54, ADR 0024).** No technical overlap: inboxes live in their own `inbox:` address space, `rename` in the session-name space, and `inbox:` is reserved from names. Both add tools; see Open questions for the count and the ADR 0024 text.
- **list_agents.** No new field; the existing local-list golden test must stay byte-equal.
- **Server instructions string.** Unchanged (it is the always-present minimum); the skill carries the guidance.

## 4. Public surface to change

1. `docs/design.md` §8: the tool list gains `rename` (listed, not counted: section 7); a `rename(name)` entry with validation, `name_held`, the pin rule and the Codex launch pattern; §3 Name rule: add the pin to `followRename`; §3 journal list: the name record; §11: the new tests.
2. `AGENTS.md` Architecture: the tool list gains `rename`, with no count; one clause pointing at ADR 0027; ADR 0024 is not edited here.
3. `plugins/crosstalk/skills/crosstalk/SKILL.md`: add `rename` to the description's tool list, a "Which tool" bullet ("`rename` sets this session's own name; use it when a brief gives you a name, especially on Codex; the harness's name no longer overrides it"), and `invalid_name`, `name_held` under Recovery.
4. `docs/how-it-works.md:16`, `docs/index.md:23`, `docs/contributing.md:41`: the count and the list.
5. `docs/faq.md` if it answers "how do I name a session" (check at build); `docs/setup.md` and `README.md` count no tools and need no change.
6. `internal/mcpserver/hints_test.go` tool list. `docs/decisions/README.md` lists no individual ADRs, so it needs no line.

## 5. Commit sequence (small, test-pinned)

Each commit lands with its test; "before" is what fails on the parent commit.

1. **Name validation.** `identity.ValidName(string) error` (or beside `IsAgentID`). Test `TestValidName`: before, the symbol does not exist; after, rejects "", 65 bytes, `a@b`, a control character, `0123456789abcdef`, `inbox:x`; accepts `CrossTalk Reviewer 3`.
2. **Journal record.** `TypeName` with retention while set. Test `TestNameRecordRetained`: before, a name record is dropped by the 24 h truncation; after, the latest survives it and replay returns it.
3. **Tool and pin.** `Agent.pinned`, the `rename` handler, `followRename` pin rule, hints. Tests (`internal/mcpserver`): `TestRenameChangesAliasKeepsAgentID`; `TestRenameNameHeldRejected` (the session keeps its alias; before, no tool); `TestRenameInvalidName`; `TestRenameSameNameStillPins`; `TestFollowRenameIgnoresHarnessNameWhilePinned` (the discriminating one: the fake harness name changes after a rename and the alias must not move; fails if the pin rule is removed); `TestUnpinnedFollowRenameUnchanged`; `TestRenameUnboundRefused`; `TestRenameCodexThreadsIndependent` (two thread ids in one process, each renamed apart); `TestRenameSubagentAllowed`; `TestRenameJournalFailureRollsBack`; the golden `TestLocalListAgentsRowsUnchanged` stays green; `hints_test.go` tool list updated.
4. **Replay.** Pin restored in `register`. Tests: `TestRenamePinSurvivesRestart` (new shim process, same session_ref, alias is the pinned name before the first publish; before, it is the harness name); `TestRenamePinHeldOnRestartRetriesWhenFree` (the pinned name is held at restart so the session takes its fallback with a notice; the holder ends; the next call, even with an empty harness name, moves the alias to the pin; before, a pinned session never retries); `TestNameRecordSurvivesOldShimRewrite` (downgrade outcome, section 8).
5. **Registry-level guard.** If review of the retire path shows `Rename` can republish a retired descriptor (the known gap in design §3 Publish order), close it here: a retired registration refuses `Rename`. Test `TestRenameAfterReleaseNameRefused`. Skip this commit only if the gap is already fixed on main when the build starts.
6. **Docs and skill.** Sections of §4. Test: the existing docs/manifest/version tests plus a check that the skill and `list_tools` name the same tools (`site_test.go` and `hints_test.go` as they stand; extend the one that compares tool lists).
7. **End-to-end proof** (build-time, not a code commit): launch a Codex session with a prompt that calls `rename`, resolve it by name from a second session, send and reply, restart the Codex thread, resolve again. Recorded in the PR body. Run live on 2026-11-10 against the installed 0.2.1 binary and an interactive Codex 0.167 session on the managed daemon (#126):

| Step | Result |
|---|---|
| Codex calls `rename("e2e-126-codex")` | PASS: `{"name":"e2e-126-codex","previous":"<Codex auto title>"}` |
| `list_agents` shows the new name | PASS |
| Claude `send_message` to the new name delivers; Codex replies | PASS |
| Codex auto title does not move the name | PASS: held across several turns and the resume; no fresh title event was seen live, the pin logic is covered by `TestFollowRenameIgnoresHarnessNameWhilePinned` |
| Name survives quitting Codex and `codex resume` of the same thread | PASS: same agent_id, same name, a send to it delivered and was answered |
| Rename to a name another live session holds | PASS: `{"error":"rejected","reason":"name_held"}`, own name unchanged |
8. **ADR 0024 amendment** (separate, by that file's writer after both branches are on main): amend its Context, Decision intro and "Rejected alternatives" entry for `rename`, and its "How it answers #43" paragraph, to say `rename` exists (ADR 0027) and that the inbox answers a different need. Listed here, not done here.

## 6. Risks

- **Name squatting and spoofing.** Any bound same-user session can take a free name, including a role name a peer expects (`coordinator`). Same exposure as taking a free fallback or harness name today (design §1 same-user non-goal); a name never carries authority (ADR 0004). The skill already says to resolve a peer with `list_agents` before the first send. Not new, slightly easier because the name can be chosen.
- **Shadowing a native session.** `resolveLocal` matches registered names first, so renaming to the name of a visible non-CrossTalk session makes that session unreachable by name until the rename ends. The alias lock cannot see native sessions. Mitigation available: reject when `Discover` shows another session with that name. Left out of the first cut (needs a discovery call in the handler); listed in Open questions.
- **Stale peers.** A peer that saved the old name gets `no_such_agent` after a rename; the existing hint tells it to run `list_agents`. Same as a harness rename today.
- **Pin versus user intent on Claude.** A user's `/rename` after a model `rename` is ignored while pinned. Mitigation: the user asks the model to rename again; Open question.
- **Retire race.** `Rename` after a retired registration could republish a descriptor (known gap in design §3). Tool calls are refused for a retiring agent, so only an in-flight call can hit it; commit 5 closes it.
- **Journal growth.** One record kept per renamed session, replaced by key; negligible.

## 7. Decided (operator rulings, 2026-11-10)

The open questions of the draft are closed by operator ruling; the build follows these.

1. **The pin survives restart and resume.** Persisted as a journal record and replayed in `register` (section 3.5).
2. **A user's Claude `/rename` does not clear the pin.** The pin always wins; the session calls `rename` again to change it (section 3.3).
3. **Names held by visible non-CrossTalk sessions are not refused in v1.** The shadowing risk (section 6) stays open; revisit if dogfood shows it.
4. **Docs list the tools instead of counting them.** Every "seven tools" or other tool count (AGENTS.md included) becomes a list with no number, so the #54 build adds its tools without a count conflict.
5. **Name rules as in section 3.2.** At most 64 bytes; a name starting with `inbox:` is reserved and rejected with `invalid_name`.

Order: #43 lands before the #54 role-inbox build; this build does not wait on #54 and edits none of its files.

## 8. Rollout

Behind no flag: a new tool with no effect on a session that never calls it. Unpinned sessions keep every current behaviour, pinned by `TestUnpinnedFollowRenameUnchanged`. No migration for upgrade. Downgrade policy: an older shim treats the unknown record type as resolved and its retention rewrite (`journal.Open`, also triggered by a partial trailing line) decodes into the old `Record` struct and drops the name record. Stated outcome: a downgrade to a shim without this tool loses the pin, and the session falls back to its harness name; running `rename` again restores it. Accepted because the tool does not exist in those versions anyway; pinned by a test that writes a name record, rewrites the journal with a decoder that lacks the field, and shows the pin gone without an error (`TestNameRecordSurvivesOldShimRewrite` asserts the loss is silent and non-fatal, not that the pin survives).

## 9. Codex review

One pass, `adversarial-review --base main` with this plan's path as focus; full output `/tmp/plan-43-codex-review.log`. Verdict: needs-attention, three findings. The plan was revised once.

1. **[high] Pinned sessions cannot retry the pin after a restart found the name held.** Fixed: sections 3.3 and the ADR now say a pinned session retries `Rename(pinned)` on every call, including with an empty harness name; commit 4 gains `TestRenamePinHeldOnRestartRetriesWhenFree`.
2. **[medium] An older shim's journal rewrite drops the unknown name record.** Fixed: section 8 states the downgrade policy (the pin is lost silently, `rename` again restores it) and commit 4 pins that outcome with `TestNameRecordSurvivesOldShimRewrite`. Not rejected, but the reviewer's "round-trip preserves the pin" test is replaced by one asserting a silent, non-fatal loss, because preserving unknown fields across versions would need a journal format change that this ticket does not justify.
3. **[medium] ADR 0027 was empty and the ADR 0024 conflict unresolved.** Fixed: the ADR is written (the first Write failed on a missing prior read, so the commit saw only the template until this revision). The ADR now names the ADR 0024 passages it reverses and says the amendment lands before the build starts; the amendment itself stays a step for that file's writer (plan §5, step 8), because one writer owns ADR 0024.
