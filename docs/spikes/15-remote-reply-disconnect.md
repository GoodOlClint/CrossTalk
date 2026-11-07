# Spike 15 (S4): ask and reply across a disconnect

- **Date:** 2026-11-07
- **Base:** main f27d828 (plan PR #45)
- **Code:** `docs/spikes/15-remote-reply-disconnect/` (throwaway, its own Go package, built by `go test`; imports production packages, edits none)
- **Gates:** plan `docs/plans/release-0.2.0.md` section 8 (message flow), row S4; precedes slice R8

## Pass line (copied verbatim from the plan, row S4)

Two state dirs, two gateways on 127.0.0.1 with different ports: kill the target gateway after injection and before the response; the asker shows `unknown`; the retry injects once (target journal dedupe); the reply reaches the original asker and never a new session that reuses the name

Fail action in the plan: Fix section 8 before R8.

## Test cases (pre-registered before any spike code)

Setup common to all cases: host A and host B each have their own state dir, one shim with a Codex-shaped harness backed by `adapter.Fake`, and one gateway bound to 127.0.0.1 on its own port. `alice` runs on A, `bob` on B. A gateway is a stand-in for the plan's gateway: it forwards `/v1/a2a/<agent_id>` to that agent's loopback A2A server and rewrites the sender into a host-scoped key `remote:<fingerprint>/<agent_id>`. Killing a gateway closes its listener and open connections; the shims and their journals stay up.

| Clause | Test | What it asserts |
|---|---|---|
| C1 kill after injection, before the response | `TestKillAfterInjectionBeforeResponse` | Bob's fake harness has exactly one write when the gateway is killed, and the gateway never wrote a response to alice's connection |
| C2 the asker shows `unknown` | `TestAskerShowsUnknown` | Alice's production send path (`wire.SendTask`) returns state `unknown` for the ask, not `accepted`, `unreachable` or `rejected` |
| C3 the retry injects once | `TestRetryInjectsOnce` | After the gateway restarts, the same message_id is sent again; the answer is accepted as a duplicate and bob's harness still has exactly one write (pre-registered "and one dedupe record" superseded: that count does not discriminate, see C3b in Results) |
| C4 the reply reaches the original asker | `TestReplyReachesOriginalAsker` | Bob's `reply` returns `accepted`; alice's harness has exactly one write, with `in_reply_to` equal to the ask's message_id; alice's journal has the receipt record |
| C5a the reply never reaches a name reuser, both sessions live | `TestReplyNeverReachesNameReuser` | A second session named `alice` on host A (another agent_id) has zero harness writes; the original alice has one |
| C5b the reply never reaches a name reuser, original gone | `TestReplyToGoneAskerNotRedirected` | The original alice is closed and a new `alice` runs; bob's reply is not `accepted`, is not journaled delivered, and the new alice has zero writes |
| C3b receiver dedupe (added after review) | `TestRetryDedupedByReceiver` | The same inbound sent to bob's receiver twice: second result is `Duplicate`, one harness write, no new dedupe record |
| X1 (not a pass-line clause) pending while the asker's gateway is down | `TestReplyPendingWhileAskerGatewayDown` | Reply while A's gateway is down is not `accepted` and not journaled delivered; after the gateway restarts, a byte-identical `reply` delivers it once |

Where today's code cannot do a clause, the spike stubs the minimum in its own package and the report names the production lines R4 and R8 must change.

## Method notes

The spike drives the production code and stubs only what slices R4, R5 and R8 will build. Production code used unchanged: `shim.Shim` (reply transaction, journal, receiver), `a2a.Server` and `a2a.Client`, `wire.Wire.SendTask` (the asker's send and settle loop), `mcpserver`, the Codex harness over `adapter.Fake`. Stubs, all in the spike package:

- the gateway: forwards `/v1/a2a/<agent_id>` to the agent's loopback server, replacing the sender with the header value `remote:<fingerprint>/<agent_id>` and the bearer with a per-host gateway credential file;
- the remote principal: `a2a.Config.ResolvePeer` returns a peer for any name shaped `remote:...`, so `Sender.AgentID` carries the host-scoped key (production needs `Sender.Host`, R4);
- the remote reply: `shim.Options.OnReply` completes the ask task, then forwards the reply through the asker's gateway and returns an error when it is not accepted, which leaves the obligation pending (production needs a remote branch in `completeReply` and `forwardReply`, R8);
- the asker's outbound journal record, written by hand with `to` = the remote key (R5 and R7 do this).

The kill is deterministic: the gateway forwards the SendMessage to bob's loopback server, waits for bob's fake harness write, then closes its listener and connections without writing the response.

## Results

Command: `go test -count=1 -v ./docs/spikes/15-remote-reply-disconnect/` (also passes under `-race`, see Gates).

| Clause | Verdict | Output that shows it |
|---|---|---|
| C1 kill after injection, before the response | PASS | `--- PASS: TestKillAfterInjectionBeforeResponse`: one harness write on bob, gateway `dropped=1 written=0`, port refuses connections |
| C2 the asker shows `unknown` | PASS | `asker saw state=unknown err=failed to send HTTP request: Post ".../v1/a2a/<bob>": EOF` and bob has one write, so unknown was the truthful answer |
| C3 the retry injects once | PASS, by task reuse | `retry state=accepted err=<nil>`; bob still has one harness write and the injected message_id equals the ask's. Bob's shim stayed up, so the retry was answered by the existing journal-backed A2A task (`internal/a2a/tasks.go:84-89` returns it, `:617-625` skips the receiver); the receiver was not reached |
| C3b receiver dedupe, task store bypassed | PASS | `TestRetryDedupedByReceiver` calls bob's receiver twice with the same sender key and message_id: the second result is `Duplicate`, bob has one harness write, and the second receive adds no dedupe record (the first leaves two: acceptance, then the outcome). Added after Codex review finding 1 |
| C4 the reply reaches the original asker | PASS | bob's `reply` returns `accepted`; alice's harness has one write with `in_reply_to` = the ask id and text `42`, on thread `T-alice`; alice's journal has the receipt record |
| C5a no reply to a second session, both live | PASS, weak | A second session on thread `T-alice-2` registered as `codex-alice-2bd36725`, not `alice`: the registry renames a live collision, so a live name reuse cannot happen. It got zero writes; the original got one |
| C5b no reply to a name reuser, original gone | PASS | the original alice is closed, a new session registers as `alice`, bob's reply is `state:unknown reason:remote_reply_unknown: unexpected HTTP status: 404 Not Found`, journaled not delivered, the new alice has zero writes |
| X1 pending while the asker's gateway is down | PASS | reply with A's gateway down: `state:unknown reason:remote_reply_unreachable: ... connection refused`, obligation not delivered; after the gateway restarts a byte-identical `reply` returns `accepted`, alice has one write, obligation delivered |

C5b is the real name-reuse test: C5a cannot reuse a live name, so it only shows a differently named second session is untouched.

## What plan section 8 must change

Nothing failed, so no clause forces a redesign. Three wording fixes, none structural:

- Section 8 step 5 and section 9 row "Connection drops after the gateway forwarded" say the asker polls `GetTask` and sees `unknown/pending`. When the connection dies before the SendMessage response (C1) the asker holds no task id, so there is nothing to poll: `a2a.Client.Send` fails with a transport error and `wire.SendTask` returns `unknown` with the raw error text as the reason (`EOF`), not `pending`. R5 should map that error to a stable reason; the retry is a resend of the same message_id (C3), not a poll.
- Section 8 reply step 3 says A's gateway answers `rejected/no_such_agent` for a gone asker. A bare HTTP 404 reaches the sender as `unknown` (C5b), and `unknown` keeps retrying until the deadline or 24 h. The gateway must answer with the A2A error that carries `metadata.crosstalk.state` and `reason`, as the loopback server does, or R8 must treat a 404 from a gateway as `rejected/no_such_agent`.
- Route lifecycle: `CompleteAsk` closes the ask task, and `OnAskClosed` (production wires it the same way, `internal/wire/wire.go`) closes bob's route before any forward. So when the forward fails the obligation is pending but the route is already closed; a retry needs no open route. Plan section 8 reply step 4 should say the retry works from the obligation alone. X1 does not assert route state.

## Production lines R4 and R8 must change

The spike reached every clause by stubbing outside production. These are the lines the stubs stand in for (line numbers at f27d828):

- `internal/shim/send.go:583-587`: a reply whose key is not an agent_id is journaled delivered and returned `accepted` with no send. A remote key must take a remote branch before this (R8, plan R4 row also notes it). The spike avoids it only because its `OnReply` returns an error until the forward is accepted.
- `internal/shim/send.go:562-563` (the hook call; `:558-561` is the missing-hook guard) and `Options.OnReply`'s signature at `internal/shim/shim.go:83`: the hook receives the original message and the reply but not the journaled sender key. The spike recovers the destination from `original.From`, which is the label and in production will be `name@host`, not a key. R8 must pass `key` (completeReply has it) to the remote forward.
- `internal/shim/send.go:612` `descriptorByID(key)` lists local descriptors only. A remote key needs a lookup by fingerprint in the peer table and a send by agent_id (R8).
- `internal/shim/send.go:637-667` `retryObligations`, called only from `internal/shim/agent.go:187` at registration: there is no timer. X1 resumed only because the test repeated `reply`. R8's 60 s doubling timer is needed for an agent that stays up.
- `internal/shim/send.go:520`: the inform-route expiry guard tests `IsAgentID(key)`, so it skips a remote key (R4 row).
- `internal/receiver/receiver.go:61-69` `key()`, `:266-267` (`askedPeer` and the receipt record use `in.Sender.AgentID`) and `:291` (route record): the spike put the host-scoped key into `Sender.AgentID` and the receipt and route worked. With a separate `Sender.Host`, these three lines must use `key()` (R4).
- `internal/a2a/server.go:443-481` `authenticate`: the spike gets the remote principal through `ResolvePeer` and a credential file. R4 adds the gateway-credential case with gateway-set host and agent headers.
- `internal/a2a/server.go:422-427` `principal`: it returns `AgentID`, so task ownership and the dedupe key were already host-scoped in the spike; with `Host` it must include it (plan section 2).

## Surprises for R8

- A gateway must forward the `A2A-Version` header. The first spike run dropped it and the target answered `rejected: this version is not supported: requested "0.3", served "1.0"` before injecting anything. R2's gateway must copy that header and every non-`X-CrossTalk-*` request header.
- The C3 retry was answered by the existing journal-backed task. The same message_id under a different principal is refused (`internal/a2a/tasks.go:85-86`, `message_id is in use by another principal`), so a gateway that derived a different fingerprint after a restart would get a refusal, not a second injection; the key must still be stable across restarts or the retry fails.
- Registry rename on a live collision (C5a) means a name can only be reused after the original is gone, which is the case the agent_id routing is for.

## Gates

`go test ./...` passes (all packages, including the spike package); `go test -race -count=1 ./docs/spikes/...` passes; `go vet ./...` is clean; `gofmt -l .` prints nothing. Run 2026-11-07 on macOS arm64, go1.27.3.
