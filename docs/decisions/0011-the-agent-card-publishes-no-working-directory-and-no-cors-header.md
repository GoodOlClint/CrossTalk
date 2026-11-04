# ADR 0011 — The Agent Card publishes no working directory and no CORS header

- **Status:** Provisional (overnight 2026-11-04); llm-council verdict recorded below
- **Date:** 2026-11-04
- **Deciders:** session (overnight), llm-council
- **Context source:** Codex adversarial review of design 1.9, finding R14; §12 open item "Agent Card exposure"

## Context

Each agent's A2A server serves an unauthenticated Agent Card (A2A 1.0 discovery document) on loopback. Today it includes the agent name, role, harness and the session's working directory, and the pinned a2a-go SDK handler answers with `Access-Control-Allow-Origin: *`, so any page in a browser on the same machine that can reach loopback, and any local process, can read session metadata without the peer token.

## Decision

The public card carries name, role, harness, version, skills and security requirements only. The working directory moves behind authentication (it stays in the descriptor directory, readable by same-user shims, and in `list_agents`). CrossTalk wraps the card handler and strips the `Access-Control-Allow-Origin` header; no cross-origin reads of local agent metadata.

## Rejected alternatives

- Authenticate the card: A2A clients expect an unauthenticated card; the peer-token scheme already protects every other method.
- Keep cwd for discovery convenience: discovery uses the descriptor directory (ADR 0003), not the card.
- Allow CORS for `127.0.0.1` origins only: browsers do not send a loopback origin a server can trust, and no browser client exists.

## Consequences

- One handler wrapper in `internal/a2a/server.go`; conformance test 17 checks the card has no cwd and no CORS header.
- Design §5 Agent Card paragraph and §12 updated; implementation commit carries `Provisional-ADR: 0011`.

## llm-council verdict

(pending)
