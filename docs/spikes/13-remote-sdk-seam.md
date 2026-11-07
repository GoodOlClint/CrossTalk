# Spike 13 — remote SDK seam (v0.2.0 S1)

- **Date:** 2026-11-07
- **Status:** PRE-REGISTERED. Results are filled in after the run; no test case below changes once code exists except to fix a bug in the test itself, and any such change is listed under Deviations.
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

To be filled in after the run.
