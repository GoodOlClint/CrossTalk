# Spike 13 — remote SDK seam (v0.2.0 S1)

- **Date:** 2026-11-07
- **Status:** run 2026-11-07, results below. Pre-registered before any spike code (commit f12de1b); no test case below changes once code exists except to fix a bug in the test itself, and any such change is listed under Deviations.
- **Plan:** `docs/plans/release-0.2.0.md` section 10 (S1), section 3.2; gates R4 and R5.
- **Code:** `docs/spikes/13-remote-sdk-seam/` (its own Go package `sdkseam`, built by `go test`; imports `internal/receiver`, `internal/journal` and `internal/adapter` unchanged; no production package is edited).

## Pass line (copied verbatim from the plan)

A gateway terminating pinned mTLS in front of an a2a-go v2.7.0 loopback server delivers a message whose `Sender` carries the host fingerprint from the connection, whatever host header the remote side sends; malformed agent fields are refused before the receiver chain; and well-formed agent and role fields, which the sending host attests and the gateway cannot check, stay inside that host's key namespace and gain no `command` authority (a remote `role: primary` still cannot command)

Fail means: rework section 3.2 (identity by header on loopback) before R4.

## Setup the cases share

Host A (client) and host B (gateway) each have an ECDSA P-256 self-signed certificate; the fingerprint is the SHA-256 of the DER. B runs a TLS 1.3 only listener on 127.0.0.1 (`RequireAnyClientCert`, `SessionTicketsDisabled`, pin check in `VerifyConnection`) that proxies `/v1/a2a/{agent_id}` to a loopback a2a-go v2.7.0 server (`a2asrv.NewJSONRPCHandler` behind an `InterceptedHandler`) with `Authorization: Bearer <gateway token>` and `X-CrossTalk-Remote-*` headers the gateway builds itself, as plan section 3.2 says. The loopback handler builds a sender from those headers and calls the real `receiver.Receiver` with the fake adapter, so "delivered" means the fake adapter recorded an inject. A is the SDK `a2aclient` over an `http.Client` that carries A's certificate.

## Clauses and test cases

Clause C0 (the premise: pinned mTLS gateway in front of the SDK server)
- `TestC0_UnpinnedClientRefusedBeforeHTTP`: a client with an unpinned certificate fails the handshake and the backend counter stays 0.
- `TestC0_TLS12Refused`: a client capped at TLS 1.2 fails the handshake.
- `TestC0_BackendRequiresGatewayToken`: a direct request to the loopback server with no bearer, a wrong bearer, and a right bearer with no remote headers is refused and nothing reaches the fake adapter.
- `TestC0_PinnedMessageDelivered`: the SDK client from the pinned host sends an inform through the gateway and the fake adapter records exactly one inject.

Clause C1 (the Sender carries the host fingerprint from the connection)
- `TestC1_SenderHostIsConnectionFingerprint`: the sender the loopback handler built has `Host` equal to A's certificate fingerprint, and the receiver's dedupe record is keyed `remote:<A fingerprint>/<agent_id>`.

Clause C2 (whatever host header the remote side sends)
- `TestC2_SpoofedHeadersIgnored`, subtests: A sends `X-CrossTalk-Remote-Host` set to a third host's fingerprint; the same with the header in lower case and mixed case; two `X-CrossTalk-Remote-Host` values; a forged `X-CrossTalk-Remote-Alias`; a forged `Authorization` (the gateway's own, as the client guesses it); a forged `X-CrossTalk-Agent` (the local-agent sender header of `internal/a2a`). In every subtest the built sender's `Host` is A's real fingerprint and its alias is the one the gateway looked up for A.

Clause C3 (malformed agent fields are refused before the receiver chain)
- `TestC3_MalformedAgentFieldsRefusedBeforeChain`, one subtest per row, each asserting a refusal and that the backend counter and the fake adapter's inject count are both 0: agent_id uppercase hex; agent_id 15 and 17 hex digits; agent_id with a non-hex digit; agent_id absent; name not base64url; name longer than 256 bytes after decoding; name containing a control character; name empty; harness `gemini`; harness absent; role `admin`; role absent; two `X-CrossTalk-Remote-Agent-ID` values.

Clause C4 (well-formed fields stay inside the host's key namespace)
- `TestC4_AttestedAgentIDStaysInHostNamespace`: A attests an agent_id equal to the id of a local agent `alice` (`a1a1a1a1a1a1a1a1`) with the same message_id as a message `alice` already delivered locally. The remote message is not a duplicate of the local one, the journal holds a dedupe record under `remote:<A fingerprint>/a1a1a1a1a1a1a1a1`, and no new record under the bare local id.
- `TestC4_PlanShapedSenderCollidesToday` (a finding, not a pass): a `receiver.Sender` shaped as plan section 3.3 reads (`AgentID` set) takes the bare agent_id as its key in today's `receiver.Sender.key()`. The test records that fact so R4 knows it must change `key()` and `registered()`.

Clause C5 (no `command` authority, a remote `role: primary` still cannot command)
- `TestC5_RemotePrimaryCannotCommand`: A attests `role: primary` and sends `kind: command`; the result is rejected with reason `policy` and the fake adapter has 0 injects. Control: a local registered primary sending the same command is accepted, so the test would fail if the receiver let a primary through.
- `TestC5_PlanShapedSenderCanCommandToday` (a finding, not a pass): the same message from a `receiver.Sender` with `AgentID` set and `Role: primary` is accepted by today's policy. The test records that R4 must add an explicit remote refusal.

## Commands

```
go test -count=1 -race -v ./docs/spikes/13-remote-sdk-seam/
go test -count=20 -race ./docs/spikes/13-remote-sdk-seam/
go vet ./... && gofmt -l .
```

## Results

Verdict: **PASS on every clause (C0 to C5).** Plan section 3.2 needs no rework; it needs two clarifications and R4 needs three code changes the spike measured (see "What the plan and R4 must change").

Run on go1.27.3 darwin/arm64, a2a-go v2.7.0, 2026-11-07. Command: `go test -count=1 -race -v ./docs/spikes/13-remote-sdk-seam/` gives `ok` with 12 top-level tests and the subtests; `go test -count=20 -race ./docs/spikes/13-remote-sdk-seam/` gives `ok` (10.3 s).

| Clause | Result | Test (all in `seam_test.go`) | What it shows |
|---|---|---|---|
| C0 pinned mTLS gateway in front of the SDK server | PASS | `TestC0_UnpinnedClientRefusedBeforeHTTP`, `TestC0_TLS12Refused`, `TestC0_BackendRequiresGatewayToken` (4 subtests), `TestC0_PinnedMessageDelivered` | An unpinned certificate fails the handshake with the gateway and backend hit counters at 0; a TLS 1.2 client fails with a protocol version error; the loopback server refuses no bearer, a wrong bearer, a right bearer with no remote headers and a right bearer with a bad host header, with 0 calls to `SendMessage`; a pinned host's inform reaches the fake adapter once |
| C1 Sender carries the connection's fingerprint | PASS | `TestC1_SenderHostIsConnectionFingerprint` | The sender the SDK interceptor built has `Host` equal to host A's certificate SHA-256, alias `host-a` from the gateway's own pin table, and the receiver's dedupe record sits under `remote:<A fingerprint>/<agent_id>` |
| C2 whatever host header the remote side sends | PASS | `TestC2_SpoofedHeadersIgnored` (7 subtests) | A forged `X-CrossTalk-Remote-Host` (upper, lower and mixed case, and two values), a forged `Remote-Alias`, a forged `Authorization` carrying the real gateway token and a forged `X-CrossTalk-Agent` all leave `Host` equal to A's real fingerprint and the alias `host-a` |
| C3 malformed agent fields refused before the chain | PASS | `TestC3_MalformedAgentFieldsRefusedBeforeChain` (14 subtests) | Each of: agent_id uppercase, 15 digits, 17 digits, non-hex, absent, twice; name not base64url, over 256 bytes, with a control character, empty; harness `gemini`, absent; role `admin`, absent is refused at the gateway (HTTP 400) with gateway forward count, backend hits, interceptor calls and adapter calls all 0 |
| C4 attested fields stay in the host's key namespace | PASS | `TestC4_AttestedAgentIDStaysInHostNamespace`, `TestC4_SameMessageIDAsLocalIsRefusedNotInjected` | A remote host that attests the id of local agent `alice` is accepted and journaled under `remote:<A fingerprint>/<alice id>`; no record appears under the bare id; alice's own record is unchanged. A remote message that reuses a local message_id is refused `duplicate_dispatch` and injects nothing |
| C5 no `command` authority | PASS | `TestC5_RemotePrimaryCannotCommand` | Remote `role: primary` with `kind: command` through the whole stack is `rejected`, reason `policy`, 0 injects; the control (a local registered primary sends the same command) is accepted, so the test does discriminate |

Two findings tests assert today's behaviour, so R4 knows what it must change (they PASS by recording the hazard; R4 should flip them):
- `TestC4_PlanShapedSenderCollidesToday`: a `receiver.Sender` with `AgentID` set, as plan section 3.3 reads, takes the bare agent_id as `key()`, so a remote message that reuses local alice's id and message_id is treated as alice's duplicate.
- `TestC5_PlanShapedSenderCanCommandToday`: the same plan-shaped sender with `Role: primary` is accepted for `command` by today's policy. The pass line holds only because the spike's sender leaves `AgentID` empty and carries the host-scoped `Principal`.

## What the plan and R4 must change

No clause failed, so section 3.2 stands. Changes found:
- Plan 3.2 says the gateway deletes every inbound `X-CrossTalk-*` header and then sets `X-CrossTalk-Remote-Agent-ID` and the other attested fields. The attested fields arrive in the very headers it deletes. The spike reads and validates the four attested headers first (exactly one value each), then deletes everything with the prefix and sets all six from validated values. Section 3.2 should say so, or name distinct inbound header names; R4 must pick one.
- A gateway refusal arrives at the SDK client as `unexpected HTTP status: 400 Bad Request`, not as an `*sdk.Error`; `internal/a2a/client.go` `errorState` maps that to `unknown`, not `rejected`. R4/R5 must make the gateway answer a JSON-RPC error body (as `internal/a2a` does for `rejection`) or map the status, or a malformed or unpinned send reads `unknown/pending` to the model.
- R4 must give `receiver.Sender` a host field and change `key()`, `registered()` and `label()` and the command policy so a remote sender is never `registered()` (both findings above). With the Principal-only shape that the spike used, `label()` returns the principal, so the injected `From` is `remote:<fingerprint>/<agent_id>` and not `name@alias`.
- The dispatch claim (`journal.DispatchState(messageID)`) is keyed by message_id alone, so a remote message that reuses a local message_id is refused `duplicate_dispatch`. It injects nothing and a UUIDv7 has 74 random bits, so the harm is a refused message at worst; R4 should key the claim by (sender key, message_id) if it wants the namespace to be complete.

## Surprises for R2 to R5

- R2: the SDK server needs nothing from the gateway beyond headers; `a2asrv.ServiceParams` exposes the six headers case-insensitively and `a2aclient.ServiceParams` lowercases the keys it sends, so case variants are one header on the wire.
- R2: the gateway used `httputil.ReverseProxy` with `Rewrite`; `Rewrite` starts from a copy of the inbound headers, so the deletion loop is needed for any `X-CrossTalk-*` header the gateway does not set itself (`Set` alone covers the six it does set; the loop was not mutation-tested).
- R5: an unpinned or unpaired client sees `remote error: tls: bad certificate` on its first read, not at dial (measured in spike 14); the sender-side mapping to `rejected/unpaired_by_peer` has to match that error.

## Deviations from the pre-registration

- `TestC4_AttestedAgentIDStaysInHostNamespace` as registered reused local alice's message_id; that case hit the `duplicate_dispatch` finding, so it is now `TestC4_SameMessageIDAsLocalIsRefusedNotInjected` and the namespace test uses a fresh message_id for the remote message. The clause verdict does not depend on the change.
- `TestC1` asserts `From` equals `remote:<fingerprint>/<agent_id>` (what the Principal-only sender labels as), not `name@alias`.
- C0 `TestC0_BackendRequiresGatewayToken` gained a fourth subtest (right bearer, bad host header).
- No mutation check was run for S1 beyond the C5 control; the S3 spike has one (spike 14).

## After slice R4

R4 (`impl-020-r4`) made the three changes listed under "What the plan and R4 must change". The two findings tests are flipped: `TestC4_PlanShapedSenderNoLongerCollides` and `TestC5_PlanShapedSenderCannotCommandNow`; the plan-shaped sender now carries `Host` and `HostAlias` as the production remote sender does. The spike's loopback names `X-CrossTalk-Remote-*` are the spike's own; production uses `X-CrossTalk-GW-*` (design section 5).
