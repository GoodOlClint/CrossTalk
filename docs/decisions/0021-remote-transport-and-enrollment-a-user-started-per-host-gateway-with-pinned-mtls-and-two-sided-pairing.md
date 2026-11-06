# ADR 0021 — Remote transport and enrollment: a user-started per-host gateway with pinned mTLS and two-sided pairing

- **Status:** Provisional (operator 2026-11-06); planned for v0.2.0, not built
- **Date:** 2026-11-06
- **Deciders:** operator + council (Claude Opus 5.5 chair, two Codex advisors) + Claude (Opus 5.5)
- **Context source:** council decision 2026-11-06 (`~/.claude/coordinators/archive/council-decision.md`, raw positions beside it); operator rulings 2026-11-06; change plan [`docs/plans/release-0.2.0.md`](https://github.com/GoodOlClint/CrossTalk/blob/main/docs/plans/release-0.2.0.md)

## Context

CrossTalk is same-machine only: each agent's A2A server binds `127.0.0.1:0` (`internal/a2a/server.go:206`), and a sender proves who it is by holding a 0600 file on the receiver's disk (`server.go:443-481`). The operator runs Claude and Codex every day on a laptop, a desktop and a Linux homelab VM, and wants their sessions to message each other over the LAN or a tailnet (Tailscale private network). Remote means the same user's own machines only, not cloud sessions and not other people. The operator ruled that any server component is opt-in and never installed or started by default, that the login unit is in v0.2.0, that `crosstalk install` gets an optional way to turn remote on, that the default port is 7745, and that v0.2.0 assumes static IPs. The pinned a2a-go v2.7.0 client takes a custom `http.Client`, but its server interceptors see only headers, so TLS must end in front of the SDK. In Go, `tls.Config.VerifyPeerCertificate` does not run on resumed connections; `VerifyConnection` does.

## Decision

- **Gateway.** One HTTPS listener per host, run by `crosstalk remote serve` in the foreground or by an opt-in login unit (`crosstalk remote enable`, or `crosstalk install --remote`, or a yes to install's interactive question, whose default is no). Nothing is installed or started by default. It binds one configured IP (`remote config --bind`, wildcards refused), port 7745 by default. Per-agent A2A servers stay on loopback unchanged; no shim listens off loopback and no shim spawns the gateway.
- **Forwarding.** The gateway authenticates the remote host, deletes every inbound `X-CrossTalk-*` header, sets its own (host fingerprint, the alias from its own peers file, the sending host's attested agent fields after validation), resolves the target agent in the local registry (never from a URL the caller gives), and forwards `SendMessage` and `GetTask` only to that agent's loopback A2A server with a gateway credential (`~/.local/state/crosstalk/gateway.token`, 0600). The loopback server runs the receiver chain as for any message; the gateway never injects.
- **Outbound.** A sending shim dials the peer's gateway directly with the host's client key and pins the peer's fingerprint.
- **TLS.** TLS 1.3 only; one self-signed ECDSA P-256 key and certificate per host (`~/.config/crosstalk/host.key` 0600, `host.crt`), used as both server and client certificate; pinning by sha256 of the public key in `VerifyConnection`; session tickets off; the pin re-checked against the current peers file on every request so `unpair` refuses requests on open connections; size, connection and per-host rate limits and whole-request, response and idle deadlines before parsing; unenrolled connections (only while a pairing is pending) in a separate pool of 4, each closed after 15 s.
- **Pairing.** Two-sided, once per pair of hosts. `crosstalk remote pair` on host A prints a token (A's bind address and port, A's fingerprint, a 32-byte secret valid 10 minutes, single use). `crosstalk remote join` on host B reads it from stdin, checks A's fingerprint, and proves the secret with an HMAC over both fingerprints, B's taken by A from the handshake, so the secret binds B's key. B sends its own bind address. A keeps the secret itself (the HMAC key) in a 0600 pending file and redeems it atomically under a lock; both record each other in `~/.config/crosstalk/peers.json` (alias, address, fingerprint). Only `POST /v1/pair` accepts an unenrolled certificate, and only while a pairing is pending. Pairing again for a known fingerprint updates its alias and address.
- **Addresses.** Static IPs: a host's address is its bind address, exchanged at pairing. A moved host shows `unreachable`; pairing again is the fix.

## Rejected alternatives

- **Gateway elected by `flock` inside whichever shim wins.** Ties the host's listener to an arbitrary session's lifetime; a pooled or hung holder never yields; no listener after the last shim exits (both advisors and the chair).
- **SSH-carried messages.** Needs inbound sshd on every Mac, and the sender's login key gives a shell, far more than messaging.
- **Tailscale identity as the base.** Does not cover a plain LAN; "same Tailscale login" is not device enrollment. May come later as an extra admission check, never the only one.
- **A relay on one host.** An always-on central router and a central impersonation point, against ADR 0001; not needed on a LAN or tailnet.
- **An always-on discovery daemon by default.** Forbidden by the operator ruling.
- **A certificate authority.** Pins are simpler for one user's few machines and need no issuance or revocation lists.
- **The pairing token as a command-line argument.** It would land in shell history.
- **`VerifyPeerCertificate` for the pin.** Skipped on resumed connections.
- **Address-change handling (mDNS refresh) in v0.2.0.** Static IPs are assumed by ruling; an mDNS broadcast that updates an already-paired fingerprint's address is v0.2.x and never adds trust.

## Consequences

- A new network listener exists whenever the user turns remote on. It is off by default, bound to one address, and refuses unpinned clients in the handshake.
- CrossTalk owns key and pairing code. Revocation is manual and per host: `unpair` on each side. A lost or reset key means pairing again with every peer; `uninstall --purge` lists them.
- `crosstalk install --check` gains K8; `uninstall` removes a login unit CrossTalk wrote (identified by a sha256 stored in `remote.json`), and `--purge` removes `~/.config/crosstalk`.
- macOS Local Network privacy and the firewall may prompt or block; spike S2 records what the user must do, and setup.md documents it.
- Spikes S1, S2, S3, S5, S6, S7 in the change plan gate the slices; a failed S7 goes to the operator, since the login unit is in v0.2.0 by ruling.
- ADR 0006 is amended to allow the user-started gateway and its login unit; ADR 0001 to name the gateway a per-host forwarder.

## Trial record

- Council verdict: remote first, user-started gateway, pinned mTLS, two-sided pairing (chair Claude Opus 5.5; advisor A Codex 630dd741ae2a2049, security; advisor B Codex d7fe688547014abc, sequencing). Operator rulings 2026-11-06: install option, login unit in v0.2.0, port 7745, static IPs.
- Rests on: ADR 0022 (the trust model the gateway feeds).
- Commits: none yet; implementing commits carry `Provisional-ADR: 0021`.
- Pinning tests (planned): `TestUnpinnedClientRefusedBeforeHTTP`, `TestUnpairRefusesOpenConnection`, `TestNoSessionResumption`, `TestPairSingleUse`, `TestPairConcurrentRedeem`, `TestPairBindsClientKey`, `TestEnableRefusesForeignUnit` (change plan section 11).
