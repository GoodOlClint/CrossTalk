# Spike 14 — remote pairing and revocation (v0.2.0 S3)

- **Date:** 2026-11-07
- **Status:** PRE-REGISTERED. Results are filled in after the run; no test case below changes once code exists except to fix a bug in the test itself, and any such change is listed under Deviations.
- **Plan:** `docs/plans/release-0.2.0.md` section 10 (S3), section 7, section 3.2; gates R3.
- **Code:** `docs/spikes/14-remote-pairing/` (its own Go package `pairing`, built by `go test`; no production package is edited).

## Pass line (copied verbatim from the plan)

Wrong or expired token and a second redemption of one token fail with nothing written; of two concurrent redemptions of one token exactly one enrolls and the other fails with nothing written; after `unpair`, a request on an already-open connection and on a resumed connection is refused

Fail means: fix section 7 before R3.

## What the prototype is

A prototype of plan section 7 steps 1 and 5: `pending.json` and `peers.json` in a temp dir, a `pending.lock` file lock (`flock`) around redemption, a redeem function that finds an unexpired entry whose secret verifies `HMAC-SHA256(secret, "crosstalk-pair-v1" || client fingerprint || server fingerprint)` in constant time, removes it, then writes the client into `peers.json`. "Nothing written" is measured as the bytes and mtime of both files being unchanged (or both files absent when they were absent). For the revocation clauses a TLS 1.3 server uses section 3.2's rules: `VerifyConnection` pin check against `peers.json`, `SessionTicketsDisabled`, and a per-request re-read of `peers.json`.

## Clauses and test cases

Clause P1 (wrong token fails with nothing written)
- `TestP1_WrongMACKnownID`: right id, MAC made with the wrong secret. Refused; `peers.json` and `pending.json` unchanged; the real redemption afterwards still succeeds (a wrong proof cannot cancel the pairing).
- `TestP1_UnknownID`: an id that is not in `pending.json`. Refused; nothing written.
- `TestP1_MACBoundToOtherFingerprint`: the MAC is valid for client fingerprint X but the handshake fingerprint is Y. Refused; nothing written.

Clause P2 (expired token fails with nothing written)
- `TestP2_ExpiredToken`: redeem one nanosecond after the expiry with the injected clock. Refused; `peers.json` unchanged. Control: one nanosecond before the expiry succeeds.

Clause P3 (a second redemption of one token fails with nothing written)
- `TestP3_SecondRedemption`: first redemption enrolls; the second, with the same valid MAC from the same client and from a different client, is refused; `peers.json` is byte-identical after the second call and holds one entry.

Clause P4 (two concurrent redemptions: exactly one enrolls, the other fails with nothing written)
- `TestP4_ConcurrentRedemptionOneWinner`: 2 goroutines, each with its own client fingerprint and a MAC valid for its own fingerprint, released together by a barrier. Exactly one returns success; `peers.json` holds exactly the winner; the loser's fingerprint is absent. Run with `-race -count=200`.
- `TestP4_ConcurrentRedemptionManyClients`: the same with 32 goroutines.
- `TestP4_ConcurrentRedemptionAcrossProcesses`: the test binary re-executes itself as 4 child processes that redeem the same token against the same directory at once; exactly one prints `enrolled`.

Clause P5 (after `unpair`, a request on an already-open connection is refused)
- `TestP5_OpenConnectionRefusedAfterUnpair`: a paired client makes request 1 (200) on a keep-alive connection; `httptrace` proves request 2 reuses that same connection; between them the server removes the client from `peers.json`; request 2 gets 403 and the handler's body did not run.

Clause P6 (after `unpair`, a request on a resumed connection is refused)
- `TestP6_ResumedConnectionRefusedAfterUnpair`: the server has `SessionTicketsDisabled: true` as in plan section 3.2, the client has a `ClientSessionCache`; after request 1 the client opens a second connection and `ConnectionState().DidResume` is false (resumption is impossible by construction), and after `unpair` a new connection fails the handshake with no handler run.
- `TestP6_ResumptionWouldStillBeRefusedByVerifyConnection` (control for the plan's Go-docs claim): the server leaves tickets on; the second connection before `unpair` has `DidResume` true; after `unpair` the resumed attempt is refused because `VerifyConnection` ran on it. This shows the pin check, not the disabled tickets, is what refuses.

## Commands

```
go test -count=1 -race -v ./docs/spikes/14-remote-pairing/
go test -count=200 -race -run 'TestP4' ./docs/spikes/14-remote-pairing/
go vet ./... && gofmt -l .
```

## Results

To be filled in after the run.
