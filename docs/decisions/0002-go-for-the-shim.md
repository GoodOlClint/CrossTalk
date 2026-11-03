# ADR 0002 — Go for the shim

- **Status:** Proposed
- **Date:** 2026-11-03
- **Deciders:** operator + Claude (Fable 5.1)
- **Context source:** operator: "an MCP server written in something like go or rust"

## Context

One shim runs per agent session, so startup time and footprint matter. It speaks MCP (stdio) to its model, websocket-over-unix JSON-RPC to the Codex daemon, a line protocol on unix sockets to Claude, and HTTP for A2A. It needs unix peer credentials (pid of the connecting process) for sender verification. The operator asked for a compiled language, Go or Rust.

## Decision

Go. Single static binary, fast start, `net` and `golang.org/x/sys/unix` cover unix sockets and peer credentials (peer pid via `getsockopt(SOL_LOCAL, LOCAL_PEERPID)` on macOS, since `xucred` carries uid/gids only; `SO_PEERCRED` on Linux), an official MCP Go SDK (`modelcontextprotocol/go-sdk`) and an A2A Go SDK exist, and iteration speed is higher than Rust for a protocol-heavy service that will change as the harnesses change.

## Rejected alternatives

- **Rust.** Same ecosystem as herdr and codex, smallest footprint, official `rmcp` SDK. Rejected for v1 on iteration speed and a less mature A2A SDK; nothing in the design prevents a later port, since every contract is on the wire.
- **TypeScript on Node.** Richest SDKs and herdr-a2a's spawn code could be reused, but one Node runtime per session is the heaviest footprint, and the operator asked for a compiled binary.
- **Python.** The spike client is Python; rejected for per-session startup cost and distribution.

## Consequences

- Repo is a Go module; build is `go build ./...`, tests `go test ./...`.
- Peer-credential code is platform-specific; macOS first, Linux behind a build tag.
- The macOS `LOCAL_PEERPID` call is verified by a committed spike (`docs/spikes/README.md`, spike 5) before the uds ingress is written.
- The MCP Go SDK's stdio transport is the model-facing surface; everything else is net/http and a small websocket client.
