# ADR 0003 — Discovery from harness registries plus a descriptor directory

- **Status:** Provisional
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1) + Codex (0.166.0) review
- **Context source:** operator question "I assume we don't need herdr anymore since we have the sockets helping with discovery?"

## Context

Agents must find each other by name. Both harnesses already publish a live registry: Claude Code writes `~/.claude/sessions/<pid>.json` (name, cwd, busy/idle, socket path) with a 0600 key file; the Codex managed daemon answers `thread/loaded/list` and `thread/read` (name, cwd, status, canAcceptDirectInput). herdr also has a session snapshot, but tying discovery to it would make any agent outside herdr invisible.

## Decision

Discovery has no daemon. Each shim writes one descriptor per agent it serves to `~/.local/state/crosstalk/agents/<agent_id>.json` (agent_id, name, harness, role, owner, pid, harness_pid, harness_started_at, instance_id, cwd, session_ref, harness_endpoint, harness_version, a2a_url, uds_path, credential_ref, installed, started_at, protocol) after its listeners are up, atomically, and removes only its own instance's file on exit. `agent_id` is a stable hash of harness and session_ref; the name is an alias set from the harness-provided session name. `list_agents` enumerates every Claude session from `~/.claude/sessions` and every loaded Codex thread from `thread/loaded/list` and `thread/read`, merges them with the descriptor directory, and prunes descriptors whose pid is dead or reused.

A shim publishes no Claude-visible record in `~/.claude/sessions/` in v1: a reply address travels in the envelope, and a pooled shim has no single pid to publish under. Claude sessions see other agents through `list_agents`.

## Rejected alternatives

- **herdr session snapshot as the directory.** Headless or non-herdr agents become invisible; herdr becomes a hard dependency of messaging.
- **A directory daemon.** Another process to keep alive and restart; only worth it for cross-machine discovery, which is out of scope.
- **mDNS on loopback.** Overkill for one machine.

## Consequences

- Registration takes two `flock`s held for its lifetime: an agent record lock keyed by agent_id and an alias lock keyed by name. A held name lock sends the newcomer to a fallback name (design §3).
- Stale-record cleanup must check pid reuse (start time), not pid existence.
