# Upgrade refresh: running shims pick up a new binary, and a Homebrew upgrade runs the install follow-up

Status: design, for operator review. No implementation code is written. Draft ADRs: [0030](../decisions/0030-the-shim-re-execs-itself-in-place-when-its-binary-is-replaced.md) (shim re-exec) and [0031](../decisions/0031-the-first-shim-on-a-new-version-runs-the-plugin-and-gateway-steps-of-an-upgrade.md) (upgrade follow-up). Evidence: [spike 20](../spikes/20-upgrade-refresh.md).

Operator request (2026-11-21), verbatim: "I'd like an upgrade to automatically refresh running sessions if possible. Also a homebrew upgrade should kick off the crosstalk install commands."

Terms: shim = the `crosstalk mcp` process a harness starts. Re-exec = the process calls `execve` on itself: same pid, same stdin and stdout, new code. Follow-up = the install steps an upgrade needs after the binary itself is replaced.

## 1. Existing state

### What #133 fixed (PR #200)

- K11 (`cmd/crosstalk/install_shims.go`), warn-only: lists running `crosstalk mcp` processes whose mapped executable (device and inode: `/proc/<pid>/exe` on Linux, `lsof -d txt` on macOS) is not the installed binary. It groups them by parent (managed Codex daemon, standalone Codex app-server, Claude session, other) and names each group's restart. It kills nothing (`TestK11KillsNothing`).
- `crosstalk install` prints the K11 advice after an upgrade and never sends `config/mcpServer/reload`, which was measured not to respawn a running shim (`docs/plans/stale-shims-133.md`).
- Install restarts the remote gateway through its login unit when the unit exists and the gateway runs another binary (`gatewayRestartStep`, `cmd/crosstalk/install.go:254`).
- K8 got clearer texts for a version mismatch with the running gateway; `docs/setup.md` got an Upgrade note.

### What K11 detects and what it does not

- Detects: a shim running a file other than the installed binary, its parent kind, and shims whose executable cannot be identified (lsof or /proc unavailable).
- Does not: refresh or signal anything; tell which version a stale shim runs; tell a shim on an old file from one on a second, deliberate install path (both are "not the installed binary"); see the plugin version (that is K1); run unless someone runs `crosstalk install` or `--check`. After `brew upgrade` nobody does, so nothing reports at all.

### How the formula is made and what it can contain

- `scripts/release-build.sh formula TAG DIST DIST_LINUX` (lines 146-217) renders `Formula/crosstalk.rb` from a heredoc: per-platform `url` and `sha256` of the release assets, `def install` with `bin.install "crosstalk"`, `def caveats` (run `crosstalk install` after install and after `brew upgrade`, `crosstalk uninstall` before `brew uninstall`), and `test do` (`crosstalk version`).
- `release.yml` job `homebrew` (lines 260-349) renders it, compares with the tap's `main`, and lands a PR on `GoodOlClint/homebrew-tap` through `land-pr` (ADR 0018, 0019).
- A formula may also have `post_install`, `service` and more, but every Ruby method runs inside Homebrew's process and its sandbox rules (section 2a). ADR 0018 rejected `post_install`.

### After `brew upgrade crosstalk`, what is stale

- The registered command `<prefix>/bin/crosstalk` is unchanged (ADR 0018), so MCP registration, the Claude allow rule, PATH and the Codex config stay correct.
- Stale: every running shim (old code); the plugin, pinned to the previous release tag (K1); the gateway, if its login unit exists.

## 2. Measured facts

### (a) Homebrew post_install is sandboxed and cannot touch `$HOME`

Source read, Homebrew 7.0.15 (`/tmp/upgrade-refresh-brew-postinstall.log`; spike 20 section 1):

- `formula_installer.rb` `def post_install`: `Sandbox.run_or_fork(... "postinstall.rb" ..., step: "running post-install")`, allowed writes: logs, the formula's rack, `etc` and `var`, the prefix link directories, `<prefix>/libexec`; then `add_install_hook_rules`.
- `sandbox.rb` `def add_install_hook_rules`: temp and cache writable, Homebrew repository write-denied, `deny_read_home`, `deny_all_network` unless the formula allows network.
- `sandbox.rb` `def deny_read_home`: denies home, or a list naming `.claude` and `.claude.json` and the shell profiles.
- The sandbox is on by default on macOS (`/usr/bin/sandbox-exec`, `extend/os/mac/sandbox.rb` `available?`) and on Linux (Landlock).

So `post_install` cannot read or write `~/.claude`, `~/.claude.json` or `~/.codex`, cannot run `claude plugin` or `codex plugin` with network, and Homebrew has no user hook after an upgrade. Alternatives considered: caveats text (today), a shim that runs the follow-up on first start on a new version (chosen, Q3), a launchd or systemd path watcher (rejected, ADR 0031), `crosstalk install` detecting the mismatch at `crosstalk mcp` start (this is the chosen option, done by the shim).

### (b) Refreshing a running session without losing work

Scratch probe, spike 20 section 2 (`docs/spikes/20-upgrade-refresh/`):

| Harness | Shim re-execs in place | Shim exits |
|---|---|---|
| Claude Code 2.1.330 | Keeps working, same pid; asks `tools/list` again on `list_changed`; the model sees and calls the new tool | Restarted on the next call (new pid, new `initialize`); tool list not asked again, new tools not offered |
| Codex 0.171.0 app-server (daemon or standalone; same code) | Keeps working, new code; does not ask `tools/list` again; new tools only in new threads | `Transport closed` on every later call in that thread; no restart until a new thread |

- Managed Codex daemon: `codex app-server daemon restart` interrupts running turns (K11 text, #133). Re-exec needs no restart of anything; each thread's shim refreshes itself. The operator ruled that CrossTalk never restarts it (Q2).
- Standalone Codex app-servers: same as the daemon; one shim per thread, each re-execs itself.
- Go SDK: a re-exec'd process can be handed the session state (`ServerSessionOptions.State`, go-sdk v1.8.0 `mcp/server.go:1418-1432`) and then accepts calls without a new `initialize` (`server.go:1942-1991`). `StdioTransport` reads ahead through `json.Decoder` (`transport.go:524`); `IOTransport` (`transport.go:160`) takes a reader that stops at the end of each line, so nothing is lost on exec.
- Identity per request (ADR 0006) survives exec: same parent pid for Claude, `_meta.threadId` per call for Codex. Journals, claims, pins and the pre-bind store are on disk.

The self-re-exec candidate holds. Exit-and-respawn fails on Codex.

## 3. Operator answers (interview 2026-11-21)

| # | Question | Answer |
|---|---|---|
| Q1 | Should the shim re-exec itself automatically when its binary is replaced? | On by default; an environment variable or `crosstalk install --no-auto-refresh` turns it off. |
| Q2 | Should anything in CrossTalk ever restart the managed Codex daemon automatically? | Never. K11 still names it as advice. |
| Q3 | What should run the post-upgrade steps after `brew upgrade` (post_install cannot)? | The first shim on the new version: takes a lock, runs the steps once, logs, tells the session. |
| Q4 | How much of install may that run without a terminal? | Plugin update and gateway restart only. Never settings, rules, PATH or MCP registration; if those need changes, tell the user to run install. |

## 4. Design

### 4.1 Shim re-exec (ADR 0030)

1. Detect: every 2 s, `stat(os.Args[0])` (the absolute registered path; a Homebrew symlink is followed) and compare device and inode with the file this process maps (reuse `procFile` and `idOf` from K11, moved where `cmd/crosstalk` and the shim both reach them).
2. Gate: only when idle (no tool call in flight, no inject or delivery running), only when `CROSSTALK_NO_AUTO_REFRESH` is unset, and only when `<new file> version` exits 0 within 5 s with a parseable version.
3. Hand over: write `ServerSessionState` (JSON) to an unlinked temp file whose descriptor is inherited (no close-on-exec), name it in `CROSSTALK_REFRESH_STATE_FD`, then `syscall.Exec(os.Args[0], os.Args, env)`. On `Exec` error, keep running and log.
4. Resume: on start with `CROSSTALK_REFRESH_STATE_FD`, read and close it, `Server.Connect(ctx, &mcp.IOTransport{Reader: lineReader(os.Stdin), Writer: os.Stdout}, &mcp.ServerSessionOptions{State: st})`, then send `notifications/tools/list_changed`. Everything else starts as on a normal start (listeners, descriptors, eager registration, Codex endpoints).
5. K11: a Codex group whose shims are current but whose threads predate the upgrade cannot be seen from the process table; the K11 text for Codex parents adds that new tools appear in new threads. Shims with refresh off stay listed with today's advice.

### 4.2 Upgrade follow-up (ADR 0031)

1. On start or resume, if `~/.local/state/crosstalk/upgrade/<version>.done` is missing and refresh is not off, try `flock(LOCK_EX|LOCK_NB)` on `~/.local/state/crosstalk/upgrade.lock`. Not taken: skip.
2. In a goroutine: for each harness that is `Found` and `SetUp`, run its existing plugin step (the `Step` already in `InstallSteps`, selected by a new step kind, not copied); run `gatewayRestartStep` when the owned unit exists and the gateway is stale. Then run `installChecks`.
3. Write `upgrade/<version>.log`; write `<version>.done` only when every step passed; release the lock.
4. Hold a one-time notice for the next tool result of the shim's own session: "CrossTalk updated to X: plugin updated, gateway restarted" or "CrossTalk X: run `crosstalk install` (K<n> failed: …)".
5. `crosstalk install` takes the lock (blocking, with a message while waiting) and writes the marker on success.
6. Formula caveats (`scripts/release-build.sh` heredoc): the plugin update runs when a session next starts; `crosstalk install` stays the manual path; `crosstalk uninstall` before `brew uninstall` is unchanged.

## 5. Conflicts with existing rules, resolved in this edit window

All ADRs here are provisional and edited in place (AGENTS.md). Each edit lands with the implementing slice, and the ADR text named here is the change:

- ADR 0006 "At runtime CrossTalk spawns nothing": a shim may run the harness CLIs and the login-unit command for the follow-up (ADR 0031). Re-exec spawns nothing.
- ADR 0016 "Each step is shown and confirmed on a terminal": the plugin and gateway steps may run unconfirmed in the follow-up; install takes the lock and writes the marker. The "Install prints that running sessions keep their old server until restarted" line changes to say they refresh on their own unless turned off.
- ADR 0018: `post_install` stays rejected, now with the sandbox evidence; the caveats and the "plugin stays at the old version until the user runs `crosstalk install`" consequence change.
- AGENTS.md Architecture: one line naming ADRs 0030 and 0031.

## 6. Slices and test bar

Each slice is one PR with its tests registered first; each test fails with its guard removed.

| Slice | Change | Pinning tests |
|---|---|---|
| R1 | Line reader + `Connect` over `IOTransport` in `cmd/crosstalk` (no behaviour change) | `TestLineReaderNoReadAhead` (two requests in one write; after reading the first, the second's bytes are still unread from the pipe); the existing MCP golden tests pass unchanged |
| R2 | Session-state hand-over and resume | `TestResumeAcceptsCallWithoutInitialize` (state fd given: `tools/call` succeeds without `initialize`, and `list_changed` is sent first); `TestNoStateRejectsCallBeforeInitialize` |
| R3 | Detect, gate and exec | `TestRefreshExecsOnInodeChange` (built test binary, replaced by rename, same pid, new version answers on the same pipe); `TestRefreshWaitsWhileBusy`; `TestRefreshSkipsBadBinary` (`version` fails: no exec, old code answers); `TestRefreshOffByEnv`; `TestRefreshKeepsIdentity` (Claude parent-pid binding and journal intact after exec) |
| R4 | `--no-auto-refresh` in install for both harnesses; K11 text | `TestInstallNoAutoRefreshWritesEnv` (Claude `-e`, Codex `env` table); `TestK11CodexNotesNewThreads` |
| F1 | Upgrade lock, marker, follow-up runner | `TestFollowUpRunsOnce` (two shims, one runs); `TestFollowUpOnlyPluginAndGateway` (a fake `Run` records commands: no `mcp add`, no settings or config write); `TestFollowUpNoMarkerOnFailure`; `TestInstallWritesMarker`; `TestFollowUpNoticeOnce` |
| F2 | Caveats text, docs (`docs/setup.md` Upgrade section, site), ADR edits of section 5 | `TestFormulaRender` updated; `TestSetupDocCoversAutoRefresh` |
| L | Live run, recorded in the plan | Homebrew upgrade on a Mac with an interactive Claude session and a daemon Codex thread open: within 10 s both shims are on the new inode (K11 PASS), Claude lists a tool added in the new version, a Codex message round trip works, the plugin is at the new tag (K1 PASS), no Codex turn was interrupted |

Rollout: R1-R4 and F1-F2 land as normal PRs; the behaviour ships in the next patch release after L passes on an rc built from main. An older shim (before R3) never refreshes; K11 keeps listing it and the release notes say one last restart is needed.

## 7. Open questions

- Interactive Claude TUI was not measured (only `claude -p`, same MCP client); slice L covers it.
- A Codex model turn after a re-exec was not run (needs auth in a scratch `CODEX_HOME`); the missing `tools/list` request is the evidence.
- Whether to propose a Codex issue upstream for refetching `tools/list` on `list_changed`, so Codex threads also see new tools: a one-line filing, not this work.
- New issue to file when this is approved: "Running shims refresh on upgrade; brew upgrade runs the plugin and gateway follow-up" (#133 stays closed).

## 8. Review disposition

Filled after the Codex review.
