# Address-change nudge (PR e of #159)

Pre-registration: the tests and the guard each one pins, committed before the code. Each test must fail with its guard removed and pass with it. The design is ADR 0021 "Address-change nudge" (operator ruling R226) with the sender choice R277.

## Design in one paragraph

`crosstalk remote config` is the only place a gateway address change is known: the gateway reads `remote.json` once at start. When a write changes `AdvertiseAddr()` or `Port` against the config read under the write lock, the same command sends one visible `inform` to every local session that has a socket, and one `inform` to every CrossTalk session listed by each paired host. The peer message uses a synthetic sender (host-stable agent_id, a name that no local session carries, harness `claude`, role `primary`); the far side sees it as `remote:<pinned fingerprint>/<id>`, so there is no forwarder, attestation or receiver change and no trust change. Local sends and peer sends run concurrently under one bounded budget.

## Tests and guards

| Test | Where | Guard it pins |
|---|---|---|
| `TestAddressChangedPredicate` | `internal/remote/nudge_test.go` | fires on a changed advertise address, on a changed port, on an advertise-to-bind fallback change; silent on alias-only, bind-only-with-same-advertise, identical rewrite, first configure |
| `TestNudgeLocalSessionsGetIt` | same | every local descriptor with a socket gets the text; one without a socket is skipped; a failing local send does not stop the others |
| `TestNudgePairedHostSessionsGetIt` | same | every CrossTalk row of every paired host gets an inform; a native (non-CrossTalk) row gets none |
| `TestNudgeUnreachablePeerGetsNothingAndDoesNotDelayLocal` | same | a peer whose listing hangs or fails gets nothing, the local nudge is delivered while that peer is still blocked, and the whole call returns within the budget |
| `TestNudgeSenderIsInformAndNeverCommand` | same | the peer message is kind `inform`, validates as a message, and no path builds a `command` |
| `TestNudgeSenderNameAvoidsLocalNames` | same | the synthetic sender name differs from every local descriptor name, and its agent_id passes attestation and is stable per host fingerprint |
| `TestNudgeTextSaysItIsNotASession` | same | both texts name `remote config` as the source, say it is not from a session, and carry the old and new address |
| `TestNudgeBoundsRowsPerPeer` | same | at most a fixed number of rows per peer are messaged |
| `TestRemoteConfigNudgesOncePerChangingWrite` | `cmd/crosstalk/remote_nudge_test.go` | one nudge per `remote config` write that changes the address, none for an unchanged rewrite, none on first configure, none when the write is refused or declined |
| `TestNudgeArrivesAsInformFromPinnedFingerprint` | `internal/wire/remote_nudge_test.go` | a nudge sent through the real remote path is injected on host 2 once, as an inform, journaled under `remote:<host 1 fingerprint>/<synthetic id>`; text that claims another sender does not change that key |
| `TestNudgeCannotCarryCommandClaimRelease` | same | a nudge-shaped message with kind `command` is rejected by the receiving host and nothing is injected |
| `TestReplyToNudgeSenderFailsCleanly` | same | a reply from host 2 to the synthetic sender (an agent_id nobody registered on host 1) comes back `no_such_agent` without a panic or a hang |

## Known gaps (stated, not solved)

- A peer this host cannot reach gets nothing; live address refresh stays deferred.
- A gateway address that changes without `remote config` (DHCP under an unchanged name or bind) is not detected.
