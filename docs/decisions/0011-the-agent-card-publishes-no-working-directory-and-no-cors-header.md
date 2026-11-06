# ADR 0011 — The Agent Card publishes no working directory and no CORS header

- **Status:** Provisional (overnight 2026-11-04); llm-council verdict recorded below
- **Date:** 2026-11-04
- **Deciders:** session (overnight), llm-council
- **Context source:** Codex adversarial review of design 1.9, finding R14; §12 open item "Agent Card exposure"

## Context

Each agent's A2A server serves an unauthenticated Agent Card (A2A 1.0 discovery document) on loopback. Today it includes the agent name, role, harness and the session's working directory, and the pinned a2a-go SDK handler answers with `Access-Control-Allow-Origin: *`, so any page in a browser on the same machine that can reach loopback, and any local process, can read session metadata without the peer token.

## Decision

The public card carries name, a description naming harness and role, `metadata.crosstalk` role and harness, version, the JSON-RPC interface URL, text modes, skills and security requirements only. The working directory is removed from the card and is not served over A2A at all; discovery reads it from the descriptor directory (ADR 0003) and `list_agents`. CrossTalk wraps the card handler: it strips every `Access-Control-*` response header and `Vary` (the pinned SDK reflects `Origin` and adds `Allow-Credentials`, and answers OPTIONS with `Allow-Methods`, `Allow-Headers` and `Max-Age`), answers OPTIONS with 204 and no CORS headers, and rejects a non-loopback `Host` header with 421 so a DNS-rebound page cannot read the card as same-origin. Residual exposure, accepted: the card route takes no bearer token and checks only `Host`, so any local process that can connect to loopback, including another OS user's, can read name, role, harness and the endpoint URL. This is metadata minimization, not isolation from local processes.

## Rejected alternatives

- Authenticate the card: A2A clients expect an unauthenticated card; the peer-token scheme already protects every other method.
- Keep cwd for discovery convenience: discovery uses the descriptor directory (ADR 0003), not the card.
- Allow CORS for `127.0.0.1` origins only: browsers do not send a loopback origin a server can trust, and no browser client exists.

## Consequences

- One handler wrapper in `internal/a2a/server.go`; `TestAgentCardExposure` (`internal/a2a/server_test.go`) sends `Origin: http://evil.example` on GET and OPTIONS and asserts no `Access-Control-*` header, no `Vary`, no `cwd` key, and that a non-loopback `Host` gets 421; the test also asserts no second route serves the card.
- Design §5 Agent Card paragraph and §12 updated; implementation commit carries `Provisional-ADR: 0011`.

## llm-council verdict (2026-11-04, overnight)

Same council as ADR 0010. Verdict: implement with stated changes, folded into the Decision above: strip every CORS header and `Vary`, cover OPTIONS and any alternate card path, test with a reflected Origin (Executor, First Principles, Contrarian); loopback `Host` allowlist against DNS rebinding (Contrarian, First Principles; the Executor called it optional); say cwd is not served over A2A at all and that name/role/harness exposure is accepted (Contrarian). No dissent on implementing.

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (card field list matches `Card`/`CardJSON`; residual exposure is any local process including other OS users; pinning test is `TestAgentCardExposure`, not design test 17, the open SDK gate). Decision unchanged. Status stays Provisional.
