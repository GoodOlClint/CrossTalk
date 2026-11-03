# Spike 5 — macOS peer pid from Go (LOCAL_PEERPID)

- **Date:** 2026-11-03
- **Host:** macOS 27.2 (26B123), arm64, Darwin 27.2.0
- **Toolchain:** go1.27.3 darwin/arm64, `golang.org/x/sys` v0.49.0
- **Code:** `internal/peercred` (`PeerPID(conn *net.UnixConn)`: `unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)` on darwin, `SO_PEERCRED` on linux)
- **Precedes:** uds ingress (design §5, ADR 0002)

## Procedure

Three tests in `internal/peercred/peercred_test.go`:

1. Same process over a unix listener: dial the listener from a goroutine, `AcceptUnix`, call `PeerPID` on the accepted connection, expect `os.Getpid()`.
2. Same process over `socketpair(AF_UNIX, SOCK_STREAM)`: expect `os.Getpid()`.
3. Other process: the test binary re-executes itself as a child that dials the listener and writes its own pid; expect `PeerPID` on the accepted connection to equal `cmd.Process.Pid` and the pid the child reported, and to differ from the parent.

Test 3 is the case the uds ingress needs (the peer is another process). Mapping the pid to a Claude session pid or to another shim's descriptor is registry work (design §5) and is not part of this spike.

## Command and output

```
$ go test -v -count=1 ./internal/peercred
=== RUN   TestPeerPIDListenerSameProcess
    peercred_test.go:65: accepted peer pid=10070 os.Getpid()=10070
--- PASS: TestPeerPIDListenerSameProcess (0.00s)
=== RUN   TestPeerPIDSocketpair
    peercred_test.go:88: socketpair peer pid=10070 os.Getpid()=10070
--- PASS: TestPeerPIDSocketpair (0.00s)
=== RUN   TestPeerPIDOtherProcess
    peercred_test.go:120: child peer pid=10071 child reported="10071\n" cmd.Process.Pid=10071
--- PASS: TestPeerPIDOtherProcess (0.01s)
PASS
ok  	github.com/GoodOlClint/crosstalk/internal/peercred	0.183s
```

## Outcome

Passed. `LOCAL_PEERPID` at level `SOL_LOCAL` returns the connecting process's pid on an accepted macOS unix socket, from Go through `golang.org/x/sys/unix`, for both a same-process and a separate-process peer. The Linux path (`SO_PEERCRED`) compiles (`GOOS=linux go vet ./internal/peercred`) and is not yet run on Linux.
