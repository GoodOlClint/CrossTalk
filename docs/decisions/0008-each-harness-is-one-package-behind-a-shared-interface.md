# ADR 0008 — Each harness is one package behind a shared interface; adding one never edits another

- **Status:** Provisional
- **Date:** 2026-11-03
- **Deciders:** operator + Claude
- **Note (2026-11-05):** [ADR 0016](0016-crosstalk-install-registers-mcp-at-the-installed-path.md) adds the optional `harness.Installer` interface (0.1.3): each harness package supplies its own install, uninstall and check steps.
- **Context source:** operator instruction during the first live run: adding a harness such as Gemini must not require rewriting the Claude or Codex support. ADR 0007 is reserved for Codex toolOutput delivery (Codex-authored).

## Context

Design §2 already says "adding a harness = one adapter: register(), inject(message, delivery), status()", and `adapter.Adapter` exists. The first build did not hold that line: the shim decides the harness from the request context with a `switch` (Claude parent pid vs Codex `_meta.threadId`), discovery reads `~/.claude/sessions` and the Codex `thread/loaded/list` in shim code, binding ambiguity handling names `codex.ErrAmbiguousEndpoint`, the mcpserver resolves callers with Claude- and Codex-specific code, and envelope rendering for Claude is called from the shim. A third harness would touch `internal/shim`, `internal/mcpserver` and `cmd/crosstalk`.

## Decision

"Pluggable" here means an in-process Go interface with a compile-time list, not a runtime plugin: every release ships all supported harnesses in the one binary (operator, 2026-11-03). The bar is that adding a harness is one new package plus one line in `cmd/crosstalk`, with no change to the existing harness packages and no harness-specific code in the shim. That holds only within the extension points the interface has today: the core selects one `NativeIngress` implementation (the first harness in the list that has one) for every agent; a second native ingress protocol and a deferred-delivery (mailbox) capability are outside the interface, while installation is the optional `harness.Installer` (ADR 0016); and `Render` is on the interface but no shared code calls it, since each injector builds its own payload. Revisit when a second native protocol is chosen. Everything a harness contributes lives behind one interface in `internal/harness`, and the shim, mcpserver and A2A code never name a concrete harness.

Final interface (`internal/harness`, implemented 2026-11-03):

```go
type Harness interface {
    adapter.Adapter // Register, Inject, Status
    Name() adapter.Harness
    // Identify names the session a call comes from without contacting the
    // harness; it runs on every call. ok=false means "not mine". An empty
    // Name means the caller is named from Discover.
    Identify(ctx context.Context, req RequestContext) (Session, bool, error)
    // Bind confirms the binding when the agent registers. An error wrapping
    // ErrAmbiguous comes with a usable Binding.
    Bind(ctx context.Context, req RequestContext) (adapter.Binding, bool, error)
    Discover(ctx context.Context) ([]Session, error)
    Render(m msg.Message, from Identity) any
    NativeReplyAddress(self Self) string
}

type RequestContext struct { ParentPID int; Meta map[string]any; Client ClientInfo; MCPSessionID string }
type Session struct { Harness adapter.Harness; SessionRef, Name, Cwd, Status string; Reachable bool; HarnessPID int; Endpoint string }
type Self struct { Binding adapter.Binding; UDSPath string }
type Identity struct { Name, Harness, Address string; Verified bool }

// Optional: the first harness in the list that implements it serves every
// agent's uds_path.
type NativeIngress interface { NewIngress(cfg IngressConfig) Ingress }
type Supersession interface { Superseded(s Session) bool } // optional: the harness can prove s ended while its MCP connection lives on (design §3)
type Noticer interface { Notice() string } // optional: authority notice sent with every delivered peer message and on pulled mailbox entries
type Installer interface { ... } // optional: install, uninstall and check steps (ADR 0016); see internal/harness/harness.go
type Ingress interface { Listen() error; Serve(ctx context.Context) error; Close() error; Files() []string }

var ErrAmbiguous error // Codex: thread loaded on more than one endpoint
```

Differences from the first sketch: binding is two calls, because the MCP server identifies the caller on every tool call (cheap, no harness contact) while the endpoint probe runs once at registration; `Harness` embeds `adapter.Adapter`, since native-name sends need `Register`; the reply address reaches `Inject` as `adapter.Delivery.ReplyAddress`, set by the shim from the target harness's `NativeReplyAddress`. mcpserver receives the harness list in `mcpserver.Options.Harnesses` (from `shim.MCPOptions`) and stores the registering call's `RequestContext` on the `Agent` for the shim's `Bind`. The uds ingress is served by an agent of any harness, because its senders are native Claude sessions; Claude's protocol server is `udsingress.NewIngress`, passed in `claude.HarnessOptions.Ingress` by `cmd/crosstalk` (the `claude` package cannot import `udsingress`, whose tests import `claude`). `internal/shim/arch_test.go` pins that `internal/shim`, `internal/mcpserver`, `internal/a2a` and `internal/wire` import no `internal/harness/<name>` package.

`RequestContext` carries what the MCP server knows about a call: parent pid, `_meta`, client info, session id. Each harness package (`internal/harness/claude`, `internal/harness/codex`, later `internal/harness/gemini`) exports a constructor returning a `Harness`; `cmd/crosstalk` lists them once in a slice, in trust order. The shim iterates that slice for Bind and Discover and otherwise only holds the `Harness` it bound. Harness-specific ingress (Claude's uds socket protocol) is likewise behind an `Ingress` constructor the harness returns, so a harness with no native socket contributes none. No non-test code in `internal/shim` or `internal/mcpserver` imports a harness package, and `internal/shim/arch_test.go` pins that in `go test`. The shim's own tests do import both harness packages (`helpers_test.go`, `shim_test.go`), so the guarantee covers production code only.

## Rejected alternatives

- Keep the `switch` on harness in the shim and add a case per harness: every harness change touches shared code and the shim's tests must know every harness.
- One interface for inject/status only (today's `adapter.Adapter`) with discovery and binding in the shim: that is the current state and is what makes Gemini a rewrite.
- Runtime plugins (Go plugin, subprocess per harness): spawning is out (ADR 0006), and a compile-time list is enough for a single binary.

## Consequences

- Binding, discovery, rendering and the native-reply address live in the two harness packages, not in `internal/shim`, `internal/mcpserver` or `internal/wire`.
- Design §2 "Components" and §6 "Injector per harness" are the contract and name the interface; a new harness is one package plus one line in `cmd/crosstalk`.
- Codex's toolOutput work (ADR 0007) implements `Render` and `Inject` for Codex and must not add Codex knowledge to the shim.

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (interface block: Identity.Harness, Noticer, Installer). Status stays Provisional.
