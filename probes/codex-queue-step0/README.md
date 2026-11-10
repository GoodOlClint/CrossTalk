Throwaway scripts for `docs/plans/codex-no-daemon-queue-step0.md`. Not product code; paths and the disposable `CODEX_HOME` are set by hand.

- `ins.sh <home> <thread> <text> [order]`: raw insert of one user-input row into a disposable `queue_1.sqlite`.
- `watch.py <db> <seconds>`: polls the queue table every 0.2 s and prints each change.
- `lockprobe.py <lockfile>`: flock and `F_GETLK` probe of a writer-lock file.
- `mcp_probe.py`: stdio MCP server that logs process facts and each call's `_meta`.
- `procfacts.go.txt`: cgo-free Go probe (parent pid, start time, executable path, `LOCAL_PEERPID`, flock); rename to `.go` in its own module to build.
