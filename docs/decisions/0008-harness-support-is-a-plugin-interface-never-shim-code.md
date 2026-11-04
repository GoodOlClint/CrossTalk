# ADR 0008 — Harness support is a plugin interface, never shim code

- **Status:** Accepted (provisional, operator 2026-11-03)
- **Date:** 2026-11-03
- **Deciders:** operator + Claude
- **Context source:** operator instruction during the first live run: adding a harness such as Gemini must not require rewriting the Claude or Codex support. ADR 0007 is reserved for Codex toolOutput delivery (Codex-authored).

## Context

Design §2 already says "adding a harness = one adapter: register(), inject(message, delivery), status()", and `adapter.Adapter` exists. The first build did not hold that line: the shim decides the harness from the request context with a `switch` (Claude parent pid vs Codex `_meta.threadId`), discovery reads `~/.claude/sessions` and the Codex `thread/loaded/list` in shim code, binding ambiguity handling names `codex.ErrAmbiguousEndpoint`, the mcpserver resolves callers with Claude- and Codex-specific code, and envelope rendering for Claude is called from the shim. A third harness would touch `internal/shim`, `internal/mcpserver` and `cmd/crosstalk`.

## Decision

Everything a harness contributes lives behind one interface in `internal/harness`, and the shim, mcpserver and A2A code never name a concrete harness.

```go
type Harness interface {
    Name() adapter.Harness                                  // "claude", "codex", "gemini"
    // Bind decides from the MCP request context whether this call comes from
    // one of its sessions; ok=false means "not mine", let the next harness try.
    Bind(ctx context.Context, req RequestContext) (adapter.Binding, bool, error)
    // Discover lists this harness's live sessions, with or without CrossTalk.
    Discover(ctx context.Context) ([]Session, error)
    Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error)
    Status(ctx context.Context, sessionRef string) (adapter.SessionStatus, error)
    // Render produces what the harness's model sees for an inbound message
    // (envelope text for Claude, toolOutput payload for Codex per ADR 0007).
    Render(m msg.Message, from Identity) any
    // NativeReplyAddress is the address a native reply tool in this harness can
    // reach, or "" when the harness has none (then replies go through `reply`).
    NativeReplyAddress(self Binding) string
}
```

`RequestContext` carries what the MCP server knows about a call: parent pid, `_meta`, client info, session id. Each harness package (`internal/harness/claude`, `internal/harness/codex`, later `internal/harness/gemini`) exports a constructor returning a `Harness`; `cmd/crosstalk` lists them once in a slice, in trust order. The shim iterates that slice for Bind and Discover and otherwise only holds the `Harness` it bound. Harness-specific ingress (Claude's uds socket protocol) is likewise behind an `Ingress` constructor the harness returns, so a harness with no native socket contributes none. Tests for the shim use `adapter.Fake` wrapped as a `Harness`; no harness package is imported by `internal/shim` or `internal/mcpserver` after the refactor, and a `go list -deps` check pins that in `go test`.

## Rejected alternatives

- Keep the `switch` on harness in the shim and add a case per harness: every harness change touches shared code and the shim's tests must know every harness.
- One interface for inject/status only (today's `adapter.Adapter`) with discovery and binding in the shim: that is the current state and is what makes Gemini a rewrite.
- Runtime plugins (Go plugin, subprocess per harness): spawning is out (ADR 0006), and a compile-time list is enough for a single binary.

## Consequences

- A refactor slice moves binding, discovery, rendering and the native-reply address out of `internal/shim`, `internal/mcpserver` and `internal/wire` into the two harness packages; behaviour is unchanged and pinned by the existing tests.
- Design §2 "Components" and §6 "Injector per harness" are the contract and name the interface; a new harness is one package plus one line in `cmd/crosstalk`.
- Codex's toolOutput work (ADR 0007) implements `Render` and `Inject` for Codex and must not add Codex knowledge to the shim.
