# Spike 18 — remote send latency and settle (v0.2.0 S5)

- **Date:** 2026-11-08
- **Status:** PARTIAL. The Mac to VM series (Wi-Fi) is PENDING-OPERATOR. Only a labelled same-host stand-in was measured.
- **Plan:** `docs/plans/release-0.2.0.md` section 10 (S5); gates R7.
- **Build:** main `3d7f55b` (ad-hoc signed `go build`). `internal/wire/remote_send.go` already sets `RemoteSettle = 5 * time.Second`; `DefaultSettle = 2 * time.Second` is the local bound.
- **Code:** `docs/spikes/16-live-real-machines/driver/` (`driver send FROM TO N`).

## Pass line (plan row S5)

Send plus the 2 s settle poll ends `accepted` on Wi-Fi and on a relayed tailnet path in 9 of 10 runs, or the settle bound remote targets need is measured.

Terms: **run** is one `send_message` call to a remote name; **ms** is the wall time of that tool call, which covers resolving the name through the peer's `GET /v1/agents`, the `SendMessage` post and the `GetTask` settle poll until the task leaves `submitted`; **accepted** is the final delivery state the tool returns when the receiving harness took the message; **min, median, p90, max** are the smallest value, the middle value (mean of the two middle values for 10 runs), the 90th percentile by nearest rank (the 9th of 10 sorted values) and the largest.

## Results

| Series | Path | Interface | Runs | min | median | p90 | max | accepted | accepted within 2 s | within 5 s (`RemoteSettle`) |
|---|---|---|---|---|---|---|---|---|---|---|
| A | Mac to VM, direct LAN | Wi-Fi (the only Mac interface up) | 0 | — | — | — | — | — | PENDING-OPERATOR | PENDING-OPERATOR |
| B | Mac to VM, direct LAN | wired | 0 | — | — | — | — | — | NOT RUN: no wired adapter is active (`en3`-`en5` inactive); the operator's network was not changed | NOT RUN |
| C | Mac to VM, tailnet | — | 0 | — | — | — | — | — | NOT RUN: no 100.x interface on the Mac | NOT RUN |
| D (stand-in) | VM gateway to VM gateway, both on the VM's LAN address, ports 7745 and 7746 | none (same host, kernel loopback path for a local address) | 10 | 108 ms | 112 ms | 121 ms | 122 ms | 10 of 10 | 10 of 10 | 10 of 10 |

Series D, per run in ms with the settle outcome: 109, 122, 110, 121, 113, 118, 111, 110, 108, 114, all `accepted`. A second kind of call in the same session, `ask` to the same agent, took 116 ms and was `accepted`.

Series D is **not** a pass for the S5 line. It measures the cost of the production send path with no network between the hosts: TLS 1.3 handshake, JSON-RPC, journal write on the receiver and the settle poll. It says nothing about Wi-Fi latency, power-saving radios or relayed paths, which the plan's pass line is about.

## What the measurement supports

- On a zero-latency path the whole remote send, including a fresh TLS handshake per call (session tickets are disabled by design) and name resolution, costs about 0.11 s. The settle poll ends at the first `GetTask` that leaves `submitted`, so it does not wait out its bound when the receiver is quick.
- That puts about 1.9 s of the 2 s local bound, and about 4.9 s of the 5 s `RemoteSettle`, to the network and to the receiving harness. On a real LAN with a sleeping Wi-Fi radio the first packet can cost hundreds of milliseconds; only series A can say whether 2 s holds.
- Recommendation for R7: **keep `RemoteSettle` at 5 s**. Do not lower it on this evidence. Revisit after series A: if its p90 is under 1 s the bound could drop to 3 s, and if any run in A ends `unknown/pending` the bound is too small for Wi-Fi.

## Surprises

- Each call opens a new TLS connection. The stand-in shows this costs little; over Wi-Fi it may not.
- `list_agents` over the same path took 2 ms in the sender's own call, which includes the remote fetch (a cached or fast path; the call waits at most 1.5 s for remote hosts by design).

## Pending for the operator session

Series A (and B, C if an interface exists): 10 runs each with `driver send <name> <peer-agent>@<alias> 10`, after the Mac filter decision in spike 16.
