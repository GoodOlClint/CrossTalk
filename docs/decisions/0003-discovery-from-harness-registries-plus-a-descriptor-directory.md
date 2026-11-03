# ADR 0003 — Discovery from harness registries plus a descriptor directory

- **Status:** Proposed
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1) + Codex (0.166.0) review
- **Context source:** operator question "I assume we don't need herdr anymore since we have the sockets helping with discovery?"

## Context

Agents must find each other by name. Both harnesses already publish a live registry: Claude Code writes `~/.claude/sessions/<pid>.json` (name, cwd, busy/idle, socket path) with a 0600 key file; the Codex managed daemon answers `thread/loaded/list` and `thread/read` (name, cwd, status, canAcceptDirectInput). herdr also has a session snapshot, but tying discovery to it would make any agent outside herdr invisible.

## Decision

Discovery has no daemon. Each shim writes one descriptor to `~/.local/state/crosstalk/agents/<name>.json` (name, harness, role, owner, pid, harness_pid, harness_started_at, instance_id, cwd, session_ref, harness_endpoint, harness_version, a2a_url, uds_path, credential_ref) after its listeners are up, atomically, and removes only its own instance's file on exit. `list_agents` reads that directory, prunes entries whose pid is dead or reused, and enriches each row with live status from the harness's own registry. herdr pane ids are recorded when present; no v1 code uses them.

A shim also publishes a Claude-visible pair in `~/.claude/sessions/` (its own pid, its own key, a distinct name, `kind: external`) so Claude sessions can address it with native SendMessage. This is conditional on spike 7 (`docs/spikes/README.md`), regression-tested by design §11 test 11; if Claude rejects or mishandles the record, discovery stays in `list_agents` only.

## Rejected alternatives

- **herdr session snapshot as the directory.** Headless or non-herdr agents become invisible; herdr becomes a hard dependency of messaging.
- **A directory daemon.** Another process to keep alive and restart; only worth it for cross-machine discovery, which is out of scope.
- **mDNS on loopback.** Overkill for one machine.

## Consequences

- Stable names are a launch-time input; two live shims with the same name is a startup conflict, enforced by a per-name `flock` held for the shim's lifetime (design §3).
- Stale-record cleanup must check pid reuse (start time), not pid existence.
- The Claude-visible record must never borrow a Claude session id or token.
