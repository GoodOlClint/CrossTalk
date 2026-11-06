# ADR 0003 — Discovery from harness registries plus a descriptor directory

- **Status:** Provisional
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1) + Codex (0.166.0) review
- **Context source:** operator question "I assume we don't need herdr anymore since we have the sockets helping with discovery?"

## Context

Agents must find each other by name. Both harnesses already publish a live registry: Claude Code writes `~/.claude/sessions/<pid>.json` (name, cwd, busy/idle, socket path) with a 0600 key file; the Codex managed daemon answers `thread/loaded/list` and `thread/read` (name, cwd, status, canAcceptDirectInput). herdr also has a session snapshot, but tying discovery to it would make any agent outside herdr invisible.

## Decision

Discovery has no daemon. Each shim writes one descriptor per agent it serves to `~/.local/state/crosstalk/agents/<agent_id>.json` (agent_id, name, harness, role, owner, pid, harness_pid, harness_started_at, instance_id, cwd, session_ref, harness_endpoint, harness_version, a2a_url, uds_path, credential_ref, installed, started_at, protocol) after its listeners are up, atomically, and removes only its own instance's file on exit. `agent_id` is a stable hash of harness and session_ref; the name is an alias set from the harness-provided session name. `list_agents` enumerates every Claude session from `~/.claude/sessions` and every loaded Codex thread from `thread/loaded/list` and `thread/read` on the known app-server endpoints (the default control socket, `CROSSTALK_CODEX_ENDPOINTS`, and the `harness_endpoint` of live Codex descriptors; a daemon on none of those is not discovered), merges them with the descriptor directory, and prunes descriptors whose pid is dead or reused.

Planned for v0.2.0, not built ([ADR 0022](0022-cross-host-addressing-and-trust-name-host-host-scoped-sender-keys-inform-ask-and-reply-only.md)): hosts are known only by pairing (`crosstalk remote pair` / `join`, [ADR 0021](0021-remote-transport-and-enrollment-a-user-started-per-host-gateway-with-pinned-mtls-and-two-sided-pairing.md)), never discovered. The agents on a paired host are listed live in `list_agents` from that host's gateway (`GET /v1/agents`, bounded at 1.5 s, CrossTalk-bound agents only, no cwd) and are never written into the local descriptor directory, whose liveness check (`registry.go` `Live`, `List`) would prune them. Host discovery (mDNS or tailnet suggestions of machines to pair) is deferred to v0.2.x.

A shim publishes no Claude-visible record in `~/.claude/sessions/` in v1: a reply address travels in the envelope, and a pooled shim has no single pid to publish under. Claude sessions see other agents through `list_agents`.

## Rejected alternatives

- **herdr session snapshot as the directory.** Headless or non-herdr agents become invisible; herdr becomes a hard dependency of messaging.
- **A directory daemon.** Another process to keep alive and restart. For cross-machine use the planned v0.2.0 gateway lists its own host's agents on request; a daemon that discovers hosts by default stays rejected (operator ruling 2026-11-06).
- **mDNS on loopback.** Overkill for one machine.

## Consequences

- Registration takes two `flock`s held for its lifetime: an agent record lock keyed by agent_id and an alias lock keyed by name. A held name lock sends the newcomer to a fallback name (design §3).
- Stale-record cleanup must check pid reuse (start time), not pid existence.
- A live shim does not prove a live session: Claude Code's `/clear` keeps the process and shim and starts a new session id. The harness that can prove a session ended says so (`harness.Supersession`, ADR 0008), readers skip such a descriptor, and its shim retires the agent and releases both locks, keeping the journal for a resume (design §3 Superseded sessions). If the agent's handlers do not stop in time, retirement removes the descriptor and releases only the alias lock; the agent lock stays held so no second writer opens the journal (`Registration.ReleaseName`). Keeping the old record running but unaddressable was rejected: it needs a second state for one record, and the exit path already keeps everything a resume needs.

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (Codex discovery limited to known endpoints; lock release on a stop timeout). Status stays Provisional.
