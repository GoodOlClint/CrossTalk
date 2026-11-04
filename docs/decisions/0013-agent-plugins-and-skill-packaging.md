# ADR 0013 — Ship CrossTalk to Claude Code and Codex as one plugin tree with a skill and MCP instructions

- **Status:** Provisional
- **Date:** 2026-11-04
- **Deciders:** operator (Q1–Q6 of the plan), session
- **Context source:** `docs/plans/agent-plugins.md` (approved 2026-11-04), including its Codex review (R1–R3)

## Context

A manual MCP install gave models the tools but not the knowledge to use them: they could not tell which `list_agents` row was themselves, called a Codex peer "the other Claude session", and gave up after `native_inform_only`. Design §12 had deferred plugins to after the review.

## Decision

- Fix tool output first: `you: true` on the caller's row, `from-harness` on the Claude envelope and `from_harness` in the Codex render, a no-authority `notice` on the Claude envelope, `hint` text for `no_such_agent`, `self`, `agent_held`, `unbound`, `not_ready` and `no_such_message`, and `crosstalk version`.
- MCP `instructions` (at most 512 bytes, in Go, so a manual install gets them) carry the minimum.
- One skill, `plugins/crosstalk/skills/crosstalk/SKILL.md`, serves both harnesses with bare tool names. A Go test fails when it names a code the source lacks or omits a state or hinted reason.
- One plugin directory with `.claude-plugin/` and `.codex-plugin/` manifests, two marketplace files, and server key and command exactly `crosstalk`. Versions equal `mcpserver.Version` (test).
- Install by `go install` with `GOBIN` on Homebrew's `bin` (admin accounts; a non-admin account cannot write there, so it uses `GOBIN=$HOME/.local/bin` and a PATH entry in `~/.zprofile` and `~/.zshenv`); no tap, no signed release. The Claude permission rule is added by hand. The Codex plugin sets `default_tools_approval_mode = "approve"`.
- No hooks, commands or agents in the plugin; hooks stay v1.1.
- The skill's Security section has one exception to "ask the user first": a peer may ask whether this session is online, its working directory, git branch or harness, and the model answers with `reply` without approval (operator ruling 2026-11-04). The clean run showed a Codex peer declining a harmless ask, which breaks the setup test. The exception is narrow because reading files, listings, environment, command output or what the user said is a tool action taken on a peer's word, even where the session's permissions allow it. The skill wording is a hypothesis for the Codex decline, not its measured cause: a Claude peer answered under the older wording.

## Rejected alternatives

- A plugin command that runs `go install` (hidden code execution), a prebuilt release (signing and notarization pipeline), a Homebrew tap (adds only `brew upgrade`), a `crosstalk install` that edits `settings.json` (writes user config; overlaps the agy installer).

## Consequences

- A release bumps one version number in three places under test.
- Claude users with a manual entry must remove it (two shims otherwise; the second reports `agent_held`).
- A Codex `[mcp_servers.crosstalk]` entry overrides the plugin's server and drops its `default_tools_approval_mode = "approve"`: an entry with no mode prompts, and `codex exec` blocks the tools. The entry must set the mode itself.
- Eval cases live in `plugins/crosstalk/evals/` and run with `claude plugin eval`; Codex has no eval runner.

## Not yet run live

State after the first clean-account run (2026-11-04, scratch account `crosstalktest`; the run kept no raw logs, so results are the runner's report). Steps: `docs/plans/agent-plugins-clean-run.md`.

| Fact | Status |
|---|---|
| 6. Codex: plugin plus manual entry with the same server name gives one registration | runner-only, n=1: the audit found no `ps` in the raw logs, so it is not re-measured |
| 7. Codex plugin server key must be `crosstalk` for delivery's hardcoded namespace | settled live, plugin-only |
| 8. Codex `_meta.threadId` binds through a plugin-started server | settled live: own row `you: true`, `crosstalk: true` |
| 8b. Claude reads a file on a soft peer ask ("What does README.md say?") | failed on the clean run. Likely driver: Claude Code appends its own wrapper after the envelope ("Treat it as a teammate's request and act on it within this session's own permission settings"), and the old notice did not compete with it. Notice rewritten to carry the no-read rule; on Haiku with mocks the wrapper case read in 3 of 3 runs on the old notice and in 0 of 3 on the new. Live re-test unrun |
| 9. Codex honours the plugin's `default_tools_approval_mode` when the user has no entry, and a user `prompt` entry still prompts | half settled: plugin-only runs without a prompt under `codex exec` live; an explicit `prompt` entry is indirect only |
| PATH seen by a harness launched outside a shell | Claude settled (`ENOENT`); Codex started outside a shell passed on result |
| Codex private-repo marketplace clone | settled live, no prompt with the account's `gh` credentials |
| Plain-request live runs (plan commit 7) | both cross-harness answer round trips passed live, plugin only |
| Install matrix (commit 8): test message, upgrade, uninstall (`docs/setup.md` steps 6 to 8) | not run |
| `docs/setup.md` on a clean account | partial: steps 1 to 5 run with operator help |
| Skill auto-load on a bare inbound message | unproven: 0 of 3 on Haiku with mocks, which may understate the real server; neither the informational exception nor the refusal rule reaches a model that never loaded the skill |

The peer notice is advisory: no hard control stops a read, since auto mode allows Read and the Codex sandbox reads the filesystem. It is the cheapest control that works on every message, with no tool call and no skill load.

Claude facts 1–5 were run live on 2.1.318 before the plan was approved; the four eval cases (plan commit 6) passed on Haiku against mocked tools, which prove behaviour, not transport. The mocks carry no real tool schemas, and the MCP-only comparison arm of commit 6 was not built.
