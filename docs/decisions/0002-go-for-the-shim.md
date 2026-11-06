# ADR 0002 — Go for the shim

- **Status:** Provisional
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1)
- **Context source:** operator: "an MCP server written in something like go or rust"

## Context

One shim process runs per harness MCP connection, so startup time and footprint matter. It speaks MCP (stdio) to its model, websocket-over-unix JSON-RPC to the Codex daemon, a line protocol on unix sockets to Claude, and HTTP for A2A. It needs unix peer credentials (pid of the connecting process) for sender verification. The operator asked for a compiled language, Go or Rust.

## Decision

Go. Single static binary, fast start, `net` and `golang.org/x/sys/unix` cover unix sockets and peer credentials (peer pid via `getsockopt(SOL_LOCAL, LOCAL_PEERPID)` on macOS, since `xucred` carries uid/gids only; `SO_PEERCRED` on Linux), an official MCP Go SDK (`modelcontextprotocol/go-sdk`) and an A2A Go SDK exist, and iteration speed is higher than Rust for a protocol-heavy service that will change as the harnesses change.

## Rejected alternatives

- **Rust.** Same ecosystem as herdr and codex, smallest footprint, official `rmcp` SDK. Rejected for v1 on iteration speed and a less mature A2A SDK; nothing in the design prevents a later port, since every contract is on the wire.
- **TypeScript on Node.** Richest SDKs and herdr-a2a's spawn code could be reused, but one Node runtime per session is the heaviest footprint, and the operator asked for a compiled binary.
- **Python.** The spike client is Python; rejected for per-session startup cost and distribution.

## Consequences

- Repo is a Go module; build is `go build ./...`, tests `go test ./...`.
- Peer-credential code is platform-specific: `internal/peercred` has a darwin and a linux implementation selected by file suffix, and other platforms return `ErrUnsupported`.
- The macOS `LOCAL_PEERPID` call was verified by spike 5 before the uds ingress was written ([report](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/spikes/05-peerpid.md), passed, including a separate-process peer), and `internal/peercred/peercred_test.go` keeps that check in `go test`.
- The MCP Go SDK's stdio transport is the model-facing surface. A2A is the a2a-go SDK's JSON-RPC and agent-card handlers hosted on net/http (`internal/a2a/server.go`); the Codex app-server client is a small hand-written websocket client (`internal/harness/codex/appserver/ws.go`). Release builds set `CGO_ENABLED=0` (`scripts/release-build.sh`).

## Council review 2026-11-06

Sat: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3 (accuracy), advisor B Codex 93c0805d291aa89e (still right, merge, withdraw). Verdict: EDIT (spike 5 evidence linked to its report and the peercred test; A2A SDK handlers named). Status stays Provisional.
