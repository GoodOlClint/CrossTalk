# ADR 0032 — OpenCode harness supports the v2 line first; v1 and Kilo are deferred

- **Status:** Provisional (every ADR in this repo is provisional by operator ruling, 2026-11-04)
- **Date:** 2026-11-23
- **Deciders:** operator + Claude (Sonnet 5.5), Codex critique
- **Context source:** [spike 21](../spikes/21-opencode.md); [change plan](../plans/opencode-harness.md); issues #272, #273

## Context

OpenCode ships two release lines. v1 (npm `opencode-ai`, the `latest` tag, 1.19.6 tested) sends no session id in MCP `_meta`, opens no socket unless the user starts it with `--port`, and has no credential unless the user sets `OPENCODE_SERVER_PASSWORD`; with `--port` any process of any local user can inject user and system text. v2 (npm `@opencode/cli`, 2.4.0 tested) sends `_meta["ai.opencode/sessionID"]` on every `tools/call`, runs one always-on authenticated service on a fixed loopback port, and records its url, pid and password in a mode-0600 `service.json`. Kilo CLI is v1-like (source read only). ADR 0006 binds identity from harness-provided evidence per request, never per process; ADR 0008 makes a harness one package.

## Decision

The first OpenCode harness package, `internal/harness/opencode`, supports v2 only. A v2 call without the session-id `_meta` is `unbound`. OpenCode v1 and Kilo are unsupported: `crosstalk install opencode` and `install --check` name what they found and say it is unsupported, and change nothing. The package keeps its service reader and API client apart from the harness methods so a later v1 or Kilo mode can reuse them. Supported versions are named in setup docs and tracked by the compatibility check of ADR 0029.

## Rejected alternatives

- **Support v1 with `--port` and a password.** The user must remember a flag and a password, a plain TUI is unreachable, and the MCP request carries no session id, so identity is per process and not per request (ADR 0006).
- **Ship a v1 plugin that records `sessionID` for the shim.** A second artifact to ship and keep compatible with a fast-moving plugin API, an unmeasured hook path, and still no credential.
- **v1 unbound with model self-identification.** A prompt-injected model could name any session.
- **One package for v1, v2 and Kilo now.** Three delivery and identity paths in the first slice for a harness whose live path is unproven.

## Consequences

- The first release reaches only v2 users; most OpenCode users are on v1 today. The operator may reverse this before slice 1 (plan section 10, Q1).
- A v1 user gets a clear refusal, not a half-working install.
- The compatibility check tracks `@opencode/cli`, not `opencode-ai`.
- Reversing it later adds a package or a mode; nothing in the v2 package blocks that.

## Trial record

- Council: none sat; one-shot Codex critique recorded in the plan's "Codex critique" section.
- Rests on: ADR 0006, ADR 0008, ADR 0029.
- Commits: none yet (plan stage).
- Pinning test: planned in the plan's slice 3 (identify table, version gate) and slice 6 (v1 refusal).
