# ADR 0005 — crosstalk mcp is a stateless forwarder; crosstalk run owns the shim

- **Status:** Superseded by [ADR 0006](0006-no-spawning-the-mcp-server-is-the-shim-and-binds-from-harness-provided-identity.md)
- **Date:** 2026-11-03
- **Deciders:** operator + Claude session (council-reviewed, Codex concurrence)
- **Context source:** design.md §9, Codex council C2 and final-read CQ5–CQ7

## Summary

This ADR made `crosstalk run --name --role [--owner] -- <harness cmd>` the shim that spawned or adopted the harness, with `crosstalk mcp --shim <socket>` as a stateless stdio forwarder to it. It was replaced the same day by [ADR 0006](0006-no-spawning-the-mcp-server-is-the-shim-and-binds-from-harness-provided-identity.md): `crosstalk mcp` is the shim and binds identity per request from harness-provided evidence, and `crosstalk run` was never shipped (`cmd/crosstalk/main.go` has no `run` command). The full text is in git history at commit 26176c8.

Council review 2026-11-06: chair Claude (Opus 5.5), advisor A Codex 3c9b659b079700f3, advisor B Codex 93c0805d291aa89e. Verdict: historical stub, status Superseded by ADR 0006 kept.
