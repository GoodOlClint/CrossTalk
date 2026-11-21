# Stale shims and gateway after an upgrade (#133)

Measured first (scratch Codex app-server 0.171.0, its own `CODEX_HOME`, no managed daemon touched): `config/mcpServer/reload` does not respawn a running `crosstalk mcp` shim. The shim kept its pid across a reload and across a later `mcpServer/tool/call`; only a new thread started the replaced binary. So install prints restart advice (K11) and never sends the reload.

Process table: `ps -axo pid=,ppid=,command=` lists `crosstalk mcp` and each parent's command on macOS and Linux, but argv[0] is the path as launched (a Homebrew symlink), not the file mapped. The mapped file's inode is the identity: Linux `stat /proc/<pid>/exe`, macOS `lsof -a -p <pid> -d txt -Fi`. A shim is current when that inode is the installed binary's inode.

## Tests, registered before the code

K11 (`cmd/crosstalk/install_shims_test.go`), each fails with its guard removed:

- `TestK11NeverFails`: a stale shim makes K11 a WARN, `installChecks` returns false.
- `TestK11MatchingVersionNotListed`: a shim running the installed binary is not listed; none stale gives PASS.
- `TestK11GroupsByParent`: stale shims group by parent kind, one line per group, with the pids.
- `TestK11NamesRestartPerGroup`: daemon group names `codex app-server daemon restart`; standalone app-server and Claude groups name restarting the session.
- `TestK11KillsNothing`: a real child process listed as stale is still alive afterwards.
- `TestClassifyShimParent`: parent command and parent's parent give daemon, standalone app-server, Claude session or other.
- `TestParseShimTable`, `TestParseLsofInode`: the ps and lsof parsers.
- `TestListShimsFindsRealProcess`: the real lister finds a started copy of the test binary and tells a replaced file from the installed one.

Install:

- `TestInstallPrintsRestartAdviceOnUpgrade`: stale shims after install print the K11 advice, and nothing sends `config/mcpServer/reload`.
- `TestInstallRestartsGatewayWhenUnitExistsAndStale`: owned login unit and a gateway on another binary gives one restart through `unitRun` after the binary step.
- `TestInstallLeavesGatewayWhenCurrent`: a gateway already on the installed binary is not restarted.
- `TestInstallNoGatewayRestartWithoutUnit`: no login unit, no restart call.

K8:

- `TestGatewayStatusWhyClasses` (`internal/remote`): refused, handshake failed and certificate mismatch listeners give three classes.
- `TestK8DialRefusedSaysPortNotBound`, `TestK8HandshakeFailedText`, `TestK8CertificateMismatchText`: the three texts; the last two name a version mismatch with the running gateway.
- `TestK8StateStringsUnchanged`: `service running, port not bound` stays the `GatewayStatus` state.

Docs: `TestSetupDocCoversStaleShims` pins the Upgrade note and the K11 and new K8 Troubleshooting text.

Added after the Codex review: `TestInstallLeavesGatewayWhenUnitRunsOtherBinary`, `TestInstallBinaryKeepsIdenticalCopy`, `TestInstallRestartsGatewayWhenBinaryUnknown`, `TestK11ReportsUnidentifiedProcesses`, `TestK11DaemonAdviceCoversOrphanedAppServer`.
