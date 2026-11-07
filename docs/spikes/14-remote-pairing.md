# Spike 14 — remote pairing and revocation (v0.2.0 S3)

- **Date:** 2026-11-07
- **Status:** run 2026-11-07, results below. Pre-registered before any spike code (commit f12de1b); no test case below changes once code exists except to fix a bug in the test itself, and any such change is listed under Deviations.
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

Verdict: **PASS on every clause (P1 to P6).** Plan section 7 needs no fix; the spike pins down four details R3 should copy.

Run on go1.27.3 darwin/arm64, 2026-11-07. Command: `go test -count=1 -race -v ./docs/spikes/14-remote-pairing/` gives `ok` (3.8 s, 12 tests plus the skipped helper); `go test -count=200 -race -run 'TestP4_ConcurrentRedemption(OneWinner|ManyClients)'` gives `ok` (2.0 s).

| Clause | Result | Test (all in `pairing_test.go`) | What it shows |
|---|---|---|---|
| P1 wrong token, nothing written | PASS | `TestP1_WrongMACKnownID`, `TestP1_UnknownID`, `TestP1_MACBoundToOtherFingerprint` | A wrong MAC, an unknown id and a MAC valid for another fingerprint are refused (`bad proof`, `unknown pairing id`, `bad proof`); both files are byte-identical with the same mtime afterwards; after the wrong proof the real redemption still succeeds |
| P2 expired token, nothing written | PASS | `TestP2_ExpiredToken` | One nanosecond after expiry is `pairing expired` with both files unchanged; one nanosecond before succeeds |
| P3 second redemption, nothing written | PASS | `TestP3_SecondRedemption` | The same client and a different client both get `unknown pairing id` the second time; `peers.json` is unchanged and holds the first client only |
| P4 two concurrent redemptions, one winner | PASS | `TestP4_ConcurrentRedemptionOneWinner` (2 goroutines), `...ManyClients` (32), `...AcrossProcesses` (4 child processes) | Exactly one call returns nil, every other returns `unknown pairing id`, `peers.json` holds exactly the winner; 200 repeats of the two in-process cases with `-race` all pass |
| P5 open connection refused after unpair | PASS | `TestP5_OpenConnectionRefusedAfterUnpair` | `httptrace` shows request 2 reused request 1's connection; after `Unpair` it gets 403 and the handler body ran once only. Control `recheck=false` (no per-request pin read): request 2 is served (200), so the per-request recheck is what refuses |
| P6 resumed connection refused after unpair | PASS | `TestP6_ResumedConnectionRefusedAfterUnpair`, `TestP6_ResumptionWouldStillBeRefusedByVerifyConnection` | With tickets disabled (plan 3.2) the second connection has `DidResume` false; after unpair a new connection fails with `remote error: tls: bad certificate` and no handler ran. Control with tickets on: the second connection resumes (`DidResume` true), and after unpair the resumption attempt is refused too, so `VerifyConnection` does run on resumed handshakes |

Mutation check: with the `flock` call in `Store.lock` removed, `go test -count=200 -run 'TestP4_ConcurrentRedemption(OneWinner|ManyClients)'` fails repeatedly with `two winners: 1 and 2` (and 1 and 3, 2 and 3, 2 and 4, 3 and 5), so the concurrency tests do detect a missing lock. The lock was restored before the final run.

## What the plan and R3 must change

Nothing fails. Details to copy into R3:
- Redemption must take a cross-process lock, not only a mutex: the gateway and a second `remote pair` or a second gateway start can touch the files at once; `flock` on `pending.lock` held across read, remove and the `peers.json` write is what gave one winner across 4 processes.
- Order the writes as plan step 5 says (remove the pending entry, then write `peers.json`), each by temp file and rename. A crash between them burns the token and enrolls nobody, which is the safe direction; R3 should say so.
- Treat expiry as `now > expires` with the clock injected; the entry stays in `pending.json` on an expired attempt (nothing written), so R3 needs a separate sweep or the expiry check on each read.
- An unpaired peer sees `remote error: tls: bad certificate` on its first read after the handshake (TLS 1.3 reports client-certificate rejection late), and a per-request 403 on an open connection. Sender-side mapping to `rejected/unpaired_by_peer` must handle both.

## Surprises for R3

- The prototype re-read `peers.json` on every handshake and request; the plan's mtime-and-size cache was not exercised, so R2/R3 must test that a same-size rewrite within one mtime tick is still seen (not covered here).
- `Unpair` here takes `pending.lock` too, so unpairing and pairing cannot interleave on `peers.json`; the plan does not say which lock unpair takes.

## Deviations from the pre-registration

- `TestP5` runs twice (subtest `recheck=true` is the clause, `recheck=false` the control); the registered list had the clause only.
- `TestP4_ConcurrentRedemptionManyClients` is the registered 32-goroutine case, in addition to the two-goroutine case; both are run at `-count=200`, not only the two-goroutine one.
- `-count=200` covers the two in-process cases only, because the cross-process case sleeps 1.5 s per run by design; the registered command line said `-run TestP4`.
