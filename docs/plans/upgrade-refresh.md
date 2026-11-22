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
| Claude Code 2.1.330, `claude -p` only | Keeps working, same pid; asks `tools/list` again on `list_changed`; the model sees and calls the new tool | Restarted on the next call (new pid, new `initialize`); tool list not asked again, new tools not offered |
| Codex 0.171.0, scratch standalone `codex app-server`, one ephemeral thread, direct `mcpServer/tool/call`, no model turn | Keeps working, new code; does not ask `tools/list` again; new tools only in new threads | `Transport closed` on every later call in that thread; no restart until a new thread |

- Not measured, and gated on slice L: the interactive Claude TUI, the managed Codex daemon, a pooled shim serving two already-bound idle threads, and the tools a Codex model turn is offered. The rows above say only what the named modes showed.
- Managed Codex daemon: `codex app-server daemon restart` interrupts running turns (K11 text, #133). Re-exec needs no restart of the daemon; each shim refreshes itself. The operator ruled that CrossTalk never restarts it (Q2).
- Go SDK: a re-exec'd process can be handed the session state (`ServerSessionOptions.State`, go-sdk v1.8.0 `mcp/server.go:1418-1432`) and then accepts calls without a new `initialize` (`server.go:1942-1991`). The live session's state is private (`server.go:1560-1575`); only `InitializeParams()` is public (`server.go:2072`). `StdioTransport` decodes in its own goroutine through `json.Decoder` (`transport.go:524-545`), which reads ahead and may hold a decoded request no handler has seen yet, so the shim needs its own `mcp.Transport` (the `Connection` interface is public, `transport.go:73`).
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

1. Trusted path: refresh is armed only when `os.Args[0]` is absolute and equals the command a harness registration names for this binary (`Installer.Registered` reads it from the config files). Otherwise the shim never refreshes and K11 keeps listing it. At start, the shim records the device and inode of that path once: the identity passed in the handoff after a refresh, or a `stat` of the path at first start. It does not run `lsof` on a tick.
2. Detect: every 2 s, `stat` the trusted path only (a Homebrew symlink is followed) and compare with the recorded identity.
3. Probe: run `<path> version --json` with a 5 s timeout. It must report a parseable version and `refresh_handoff` equal to a format this binary writes. A binary that lacks the field (any release before R2, so every downgrade across that boundary) means no exec. Right before exec, `stat` the path again; a different inode from the probed one means probe again on the next tick.
4. Quiesce, in this order: stop admitting new work from peers (close the A2A listener and the unix socket; a peer send then gets `unreachable` and is retried by its sender's journal); stop the stdin reader at a frame boundary (the shim's own `Connection` reads stdin itself, one byte at a time up to `\n`, under a lock it holds while a partial frame is buffered); wait until every request read from stdin has had its response written (the `Connection` tracks request ids from `Read` to the `Write` of the matching response, and `Write` returns only after the bytes are in the pipe), and every inject or delivery has finished. If quiescence takes over 10 s, reopen the listeners, resume reading and try on a later tick.
5. Hand over: the handoff is a versioned JSON document (`refresh_handoff: 1`) in an unlinked temp file whose descriptor is inherited, named by `CROSSTALK_REFRESH_STATE_FD`. It holds the MCP session state the `Connection` captured from the wire: the `initialize` request params, the protocol version from the server's `initialize` response, `notifications/initialized` seen, and the last `logging/setLevel`. It also holds every agent the process had bound (agent_id, harness, session_ref, name, pin) and the recorded binary identity. Then `syscall.Exec(path, os.Args, env)`. On `Exec` error, undo step 4 and log.
6. Resume: read and close the handoff, then re-register each bound agent before serving stdin. Rewrite its descriptor with the new process's endpoints (the old descriptor still passes `Registry.Live`, because pid and start time are unchanged, `internal/registry/registry.go:415`). Hold each agent's journal lock. Then connect with `ServerSessionOptions.State` built from the handoff and send `notifications/tools/list_changed`. The entry point stays `mcpserver.Server.Run` with an options argument, so the superseded-retirement ticker and `Close` on disconnect (`internal/mcpserver/server.go:107-126`) are kept.
7. K11: shims with refresh off or not armed stay listed with today's advice. For Codex parents the text adds that tools new in this version appear only in threads started after the upgrade.

### 4.2 Upgrade follow-up (ADR 0031)

1. On start or resume, when refresh is armed and any of `~/.local/state/crosstalk/upgrade/<version>/<harness>.done` is missing for a harness that is `Found` and `SetUp`, try `flock(LOCK_EX|LOCK_NB)` on `~/.local/state/crosstalk/upgrade.lock`. Not taken: skip. Taken: read the markers again under the lock and do only what is still missing.
2. In a goroutine, for each such harness: run its existing plugin step (the `Step` already in `InstallSteps`, picked by a step kind, not copied), then verify K1 (the installed plugin is at the binary's version). Run `gatewayRestartStep` when the owned unit exists and the gateway runs another binary, then verify the gateway holding `gateway.lock` runs the installed binary. No other check runs: K6 (which starts the registered command) and K7 (which starts a login shell) are for a manual install only.
3. Write `<harness>.done` (and `gateway.done`) only for the targets whose step and verification passed. Write the log `upgrade/<version>/follow-up.log`, then release the lock. A failed target is retried the next time a shim starts on this version, and by the lock holder every 15 minutes for 6 hours. This covers a marketplace cache that still has the old ref; ADR 0019 already lands the marketplace ref before the formula moves.
4. Hold a one-time notice for the next tool result of the shim's own session: what was updated, or "run `crosstalk install`" with the failed target and its error.
5. `crosstalk install` takes the lock (blocking, with a message while waiting) and writes the marker only for the targets it installed and verified.
6. Formula caveats (`scripts/release-build.sh` heredoc): the plugin update runs when a session next starts; `crosstalk install` stays the manual path; `crosstalk uninstall` before `brew uninstall` is unchanged.

### 4.3 Shim state across a refresh

Every in-memory field of a running shim is either carried in the handoff, rebuilt from disk the same way a normal start rebuilds it, or dropped with the reason given. Field names are as of main @ cb603d0.

Carried in the handoff:

| Where | Field | Why carried |
|---|---|---|
| MCP session (go-sdk) | initialize params, negotiated protocol version, initialized, log level | The client does not repeat `initialize` (spike 20) |
| `mcpserver.Agent` | `id`, `harness`, `sessionRef`, `req` (parent pid or thread id), `harnessName` | Re-registration must happen before any call (review finding 2) |
| `mcpserver.Agent` | `viaCall` | `RetireSession` leaves a call-bound agent alone; losing it would let the Claude poll retire a session bound by a call |
| `mcpserver.Agent` | `pendingNotice` | A notice not yet attached to a tool result would be lost |
| `shim.AgentState` | `binding`, `boundAt` | `boundAt` picks the pre-bind hint for `no_such_message` (`internal/shim/send.go:735`) |
| `codex.Claims` (ADR 0028) | each proven `Binding` (its `Claim`, `Live`) | See ADR 0028 below |

Rebuilt from disk, as at every start: the journal and its replay (dedupe, routes, reply obligations, `PinnedName`); ask deadline timers (`rearmDeadlines`, `internal/shim/send.go:541`, from journaled `sent_at` plus `deadline_seconds`); the pre-bind store; descriptors, the agent flock, the unix socket and the A2A listener, opened again with new endpoints; wake generations (`codex.Wakes`, one file per thread; replay writes no row, ADR 0028 item 4).

Dropped, safe because a restart drops them today:

| Where | Field | Why safe |
|---|---|---|
| `shim.AgentState` | `sends` (per-peer counts) | ADR 0014: the nudge count resets on restart |
| `shim.AgentState` | `retrying` | Reply obligations are journaled; replay drives the retry (`OnReply` runs again on replay) |
| `shim.AgentState`, `mcpserver.Agent` | `up`, `closed`, `ready`, `draining`, `calls`, `callMu`, `replyMu`, `drainMu` | Lifecycle and locks; zero when quiescent |
| `mcpserver.Server` | `timedOut` (retired agents kept to hold their flock until exit) | Exec ends them as process exit does |
| `codex.Adapter` | `bindings`, `rebinding`, `lookedUp` (thread to app-server endpoint) | `Inject` looks the thread up again on a miss |
| `codex.Adapter` | `backends`, `probes` (ADR 0025 reachability cache) | Probed again on first use |
| `codex.ClaimServer` | `failed` (durable claims already tried) | A failed durable claim is tried once more; its proof fails again unless it now holds |
| `codex.Wakes` | `threads`, `replayed` | Per-thread mutexes; replay is idempotent |
| queue gate | Codex version cache per (parent pid, start time) | Recomputed; the schema is re-checked per write anyway (ADR 0028 item 5) |
| Claude harness | `ServeClaims` log dedupe (`readFailed`, `failed`) | Logging only |

ADR 0028 (Codex `--no-daemon` queue wake, experimental, off by default): a claim-serving shim holds proven claims in `codex.Claims.byID`, and the `delivery_mode` on its own `list_agents` row is derived from them (`DeliveryQueueWake`, `internal/harness/codex/harness.go:108`). The handoff carries each proven binding. On resume, each one is proven again by the target check of ADR 0028 item 8 (the shim's parent, unchanged by exec, still owns the thread's writer lock, and no other process has it open). A binding that fails is dropped and its agent retired, as on a recheck failure today. The live-origin proof (the hook's peer credential) is not repeated, because the handoff reaches the new image through an inherited descriptor that no other process can write. A durable claim file that is not in the handoff goes through the normal start scan, which is what a restart does today. The claim socket is closed during the exec gap. A SessionStart hook that dials in that gap fails, but it writes its durable 0600 claim file before it dials (`internal/harness/codex/hook.go:106-110`), and the resumed scan picks that file up. Quiescence also waits for a running claim proof and an open wake transaction (`Wakes` holds its cross-process lock).

## 5. Conflicts with existing rules, resolved in this edit window

All ADRs here are provisional and edited in place (AGENTS.md). Each edit lands with the implementing slice, and the ADR text named here is the change:

- ADR 0006 "At runtime CrossTalk spawns nothing": a shim may run the harness CLIs and the login-unit command for the follow-up (ADR 0031). Re-exec spawns nothing.
- ADR 0016 "Each step is shown and confirmed on a terminal": the plugin and gateway steps may run unconfirmed in the follow-up; install takes the lock and writes the marker. The "Install prints that running sessions keep their old server until restarted" line changes to say they refresh on their own unless turned off.
- ADR 0018: `post_install` stays rejected, now with the sandbox evidence; the caveats and the "plugin stays at the old version until the user runs `crosstalk install`" consequence change.
- ADR 0028: item 8 gains one sentence. A refresh carries proven claims in the handoff and proves each one again by the target check only; a claim not in the handoff takes the normal start scan (section 4.3). Item 4 gains one sentence: an open wake transaction is part of refresh quiescence.
- AGENTS.md Architecture: one line naming ADRs 0030 and 0031.

## 6. Slices and test bar

Each slice is one PR with its tests registered first; each test fails with its guard removed.

| Slice | Change | Pinning tests |
|---|---|---|
| R1 | The shim's own `mcp.Transport`: stdin read byte by byte to `\n`, request ids tracked from `Read` to the response `Write`, initialize, initialized and setLevel captured from the wire; `mcpserver.Server.Run` takes it (no behaviour change) | `TestConnNoReadAhead` (two requests in one write: after the first is returned, the second's bytes are still in the pipe); `TestConnFragmentedFrame` (a request split across writes is not quiescent until whole); `TestConnPendingUntilResponseWritten` (a handler done but its response blocked on a full pipe is not quiescent); `TestConnCapturesState` (a real `initialize`, `initialized` and `setLevel` over one connection give the captured state); `TestRunKeepsRetirementAndClose` (the superseded-retirement tick runs while connected, `Close` on EOF); the MCP golden tests pass unchanged |
| R2 | Versioned handoff, `version --json` with `refresh_handoff`, resume | `TestResumeAcceptsCallWithoutInitialize` (captured from a real handshake, then resumed: `tools/call` works, `list_changed` sent first, negotiated version and log level kept); `TestNoHandoffRejectsCallBeforeInitialize`; `TestResumeReRegistersBoundAgents` (the descriptor carries the new endpoints before the first stdin read); `TestResumeKeepsViaCallAndNotices` (a call-bound agent is not retired by the Claude poll after refresh; a held notice reaches the next tool result); `TestResumeKeepsBoundAtHint`; `TestResumeReprovesCodexClaims` (a carried claim whose lock is still parent-owned stays bound with `delivery_mode` queue wake; one whose lock is now held by another process is dropped and its agent retired); `TestResumeRearmsDeadlines` (an ask deadline set before refresh fires after it) |
| R3 | Arm, detect, probe, quiesce, exec | `TestRefreshExecsOnInodeChange` (built test binary replaced by rename: same pid, new version answers on the same pipe); `TestRefreshNotArmedForRelativeArgv0`; `TestRefreshNotArmedForUnregisteredPath`; `TestRefreshNoLsofOnTick` (the tick only stats); `TestRefreshWaitsWhileBusy` (in-flight call, partial frame, running inject, running claim proof, open wake transaction); `TestRefreshSkipsBadBinary` (`version` fails: no exec, old code answers); `TestRefreshSkipsBinaryWithoutHandoff` (downgrade: a binary whose `version --json` lacks `refresh_handoff` is not exec'd); `TestRefreshInodeChangedAfterProbe`; `TestRefreshOffByEnv`; `TestRefreshInboundBeforeAnyCall` (right after refresh, before any tool call, an inform reaches a bound Claude agent and two bound idle Codex threads on one shim) |
| R4 | `--no-auto-refresh` in install for both harnesses; K11 text | `TestInstallNoAutoRefreshWritesEnv` (Claude `-e`, Codex `env` table); `TestK11CodexNotesNewThreads` |
| F1 | Upgrade lock, per-target markers, follow-up runner | `TestFollowUpRunsOnce` (two shims, one runs); `TestFollowUpRechecksMarkerUnderLock` (forced interleaving: marker written between the first check and the lock; the second run does nothing); `TestFollowUpOnlyPluginAndGateway` (a fake `Run` records commands: plugin and unit commands only; no `mcp add`, no settings or config write, no registered command or login shell started); `TestFollowUpNoMarkerWhenK1Fails` (plugin command succeeds but installs the old version: no marker, retried); `TestFollowUpPerHarnessMarkers` (Claude-only install leaves the Codex target pending); `TestInstallWritesMarkerForItsTargets`; `TestFollowUpNoticeOnce` |
| F2 | Caveats text, docs (`docs/setup.md` Upgrade section, site), ADR edits of section 5 | `TestFormulaRender` updated; `TestSetupDocCoversAutoRefresh` |
| L | Live run, recorded in the plan | Homebrew upgrade on a Mac with an interactive Claude TUI session, a managed-daemon Codex thread and a pooled shim with two bound idle Codex threads open: within 10 s every shim is on the new inode (K11 PASS); an inform reaches each before any tool call; Claude lists a tool added in the new version; a Codex model turn works with the new code; the plugin is at the new tag (K1 PASS); no Codex turn was interrupted. Unattended plugin commands: while the follow-up runs `claude plugin marketplace update` and `claude plugin update`, and `codex plugin marketplace upgrade` and `codex plugin add`, keep the Claude session and the Codex daemon thread in a running turn. Check that both turns complete. No prompt or dialog appears in either. `~/.codex/config.toml` keeps its `[mcp_servers.crosstalk]` table byte for byte (`codex plugin add` rewrites the file). The daemon keeps serving its threads (same pid, `codex mcp list` succeeds). The Claude session's skill still loads (`/skills`) |

Rollout: R1-R4 and F1-F2 land as normal PRs; the behaviour ships in the next patch release after L passes on an rc built from main. An older shim (before R3) never refreshes; K11 keeps listing it and the release notes say one last restart is needed.

## 7. Open questions

- Interactive Claude TUI was not measured (only `claude -p`, same MCP client); slice L covers it.
- A Codex model turn after a re-exec was not run (needs auth in a scratch `CODEX_HOME`); the missing `tools/list` request is the evidence.
- Whether to propose a Codex issue upstream for refetching `tools/list` on `list_changed`, so Codex threads also see new tools: a one-line filing, not this work.
- New issue to file when this is approved: "Running shims refresh on upgrade; brew upgrade runs the plugin and gateway follow-up" (#133 stays closed).

## 8. Review disposition

One Codex adversarial pass over 52be870 (the standing Codex reviewer session, over CrossTalk). Twelve findings, each fixed in this revision:

| # | Sev | Finding | Disposition |
|---|---|---|---|
| 1 | high | A line reader and an idle handler count do not make exec safe: the SDK decoder goroutine may hold a decoded or partial request, and a response may still be unwritten | Fixed: the shim owns its `mcp.Transport`; quiescence closes peer ingress, stops at a frame boundary and waits for each response `Write` (4.1 step 4); R1 tests fragmented frames and a blocked response with the real SDK |
| 2 | high | After exec the old descriptor still passes `Registry.Live` (same pid and start time), so Claude `ServeClaims` skips registration and idle Codex threads are never rebound | Fixed: the handoff lists every bound agent and resume re-registers each, with new endpoints, before serving (4.1 steps 5-6); `TestRefreshInboundBeforeAnyCall` and slice L |
| 3 | high | A downgrade to a binary from before the handoff passes `version` but drops the restored session | Fixed: versioned handoff; exec only when `version --json` reports a matching `refresh_handoff` (4.1 step 3); `TestRefreshSkipsBinaryWithoutHandoff` |
| 4 | high | Running all install checks from the shim starts the registered command (K6) and a login shell (K7) unattended | Fixed: the follow-up verifies only K1 and the gateway's binary (4.2 step 2); F1 test asserts no registered command or shell is started |
| 5 | high | `argv[0]` may be a bare name; stat and exec of it resolve against the working directory | Fixed: refresh is armed only for an absolute `argv[0]` equal to a registered command, and the inode is re-checked between probe and exec (4.1 steps 1 and 3) |
| 6 | high | Plugin commands can install the previous tag if the marketplace ref lags, and the marker is written anyway | Fixed in part: K1 must pass before the marker, with retries (4.2 steps 2-3). The lag itself is already bounded by ADR 0019 (the `homebrew` job needs the `marketplace` job), so the residual case is a stale local marketplace cache |
| 7 | medium | The live session state is private; R2's test can pass without capturing a real handshake | Fixed: the shim's `Connection` captures initialize, its response's protocol version, initialized and setLevel from the wire; R1 and R2 test a real handshake on one connection |
| 8 | medium | Checking the marker only before `flock` allows two runs | Fixed: re-check under the lock (4.2 step 1); `TestFollowUpRechecksMarkerUnderLock` |
| 9 | medium | One shared marker lets a single-harness install claim the whole version | Fixed: per-target markers (4.2 steps 1, 3, 5); `TestFollowUpPerHarnessMarkers` |
| 10 | medium | Moving to SDK `Connect` can drop the retirement ticker and `Close` | Fixed: the entry point stays `mcpserver.Server.Run` (4.1 step 6); `TestRunKeepsRetirementAndClose` |
| 11 | medium | The spike covered `claude -p` and a scratch standalone app-server, not the daemon, pooling or a model turn | Fixed: table rows name the modes observed; the unmeasured modes are listed and gated on slice L (section 2b) |
| 12 | medium | Reusing `procFile` every 2 s runs `lsof` on every tick in every shim | Fixed: identity recorded once per process image, the tick only stats the path (4.1 steps 1-2); `TestRefreshNoLsofOnTick` |
