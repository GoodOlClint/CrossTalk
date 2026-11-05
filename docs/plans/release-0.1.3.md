# Change plan: CrossTalk 0.1.3 (crosstalk install, Linux, small fixes, Windows map)

Status: draft for the brownfield gate, 2026-11-05, revised after the Codex critique (section 9: 4 findings, all fixed). No implementation until the operator approves. Evidence tags: VERIFIED-LIVE (run on this Mac today: Claude Code 2.1.318, codex-cli 0.166.0, Go 1.27.3), VERIFIED-SOURCE (read in this repo at 3122f22), VERIFIED-RUN (from the 2026-11-05 Linux run report), UNKNOWN.

Terms: shim = the `crosstalk mcp` process a harness starts for one MCP connection. Harness = the agent CLI (Claude Code, Codex). Adapter = a harness package behind `harness.Harness` (ADR 0008). Bin dir = the directory `crosstalk install` copies the binary into. Allow rule = an entry in `permissions.allow` of `~/.claude/settings.json`. Slice = one small commit with its own test. DoD = definition of done: what must be true, evidenced, for the slice to count. Fails-before / passes-after = the named test fails on the parent commit and passes on the slice's commit. S / M / L = under 1 hour / about half a day / more than half a day.

## 1. Goal and scope

0.1.3 lets a stranger on macOS or Linux go from a downloaded binary to working CrossTalk with one command, `./crosstalk install`, and adds Linux release assets. Scope: (a) install, uninstall, reinstall; (b) Linux; (c) four small items; (d) a Windows map with a fold-in or separate-phase recommendation. Out of scope: implementing Windows (section 7 recommends a separate phase), design §11 test 17, orphaned shims (Linux finding F3), remote A2A.

## 2. Existing state (map)

| What | Where | Bearing on 0.1.3 |
|---|---|---|
| Harness contract | `internal/harness/harness.go:63-80` (`Harness`), optional `NativeIngress` (114-116) and `Supersession` (122-126) | Install becomes one more optional interface, `Installer`, in the same file. ADR 0008:14 says "installation hooks are outside the interface"; edited in place |
| Harness list | `cmd/crosstalk/main.go:62-75` builds `[]harness.Harness{cx, cl}`; subcommands at 41-57 | `install`, `uninstall`, `reinstall` join the switch; `harnesses()` is reused to reach each `Installer` |
| Claude adapter | `internal/harness/claude/harness.go:42-51` (`NewHarness`), 70-90 (`Identify` reads `~/.claude/sessions/<ppid>.json`); `claude.go:77-83` `DefaultSessionsDir` | New `install.go` in this package: plugin, MCP, allow-rule steps and checks |
| Codex adapter | `internal/harness/codex/harness.go:44-53`, 64-70 (`_meta.threadId`); `appserver/client.go:21` control socket; `appserver/methods.go:203` hardcodes namespace `crosstalk` | New `install.go`: plugin, `config.toml` table, checks. Server key must stay `crosstalk` |
| Registry and state | `internal/registry/registry.go:40-46` (`~/.local/state/crosstalk`), 71-90 (`tryLock`, `unix.Flock` at 82) | Uninstall leaves the state dir. Line 82 is the first Windows build blocker |
| Plugin tree (ADR 0013) | `plugins/crosstalk/.claude-plugin/plugin.json` and `.codex-plugin/plugin.json` (`"mcpServers": "./claude.mcp.json"` / `"./codex.mcp.json"`), both `.mcp.json` files run bare `crosstalk mcp`; marketplaces `.claude-plugin/marketplace.json`, `.agents/plugins/marketplace.json` | The server moves out of the plugin (ADR 0016). `cmd/crosstalk/plugin_test.go:80-97` (`TestServerKeysAndCommands`) pins the current server and is replaced |
| Tool prefix | 8 eval graders under `plugins/crosstalk/evals/*/graders/` name `mcp__plugin_crosstalk_crosstalk__*`; setup.md:29,36,40 | Prefix becomes `mcp__crosstalk__*` |
| Release | `.github/workflows/release.yml:36-37` builds into `$RUNNER_TEMP/dist`; signing 38-66, notarize 69-92, publish 102-120 lists two files at 107; `scripts/release-build.sh` subcommands `check-tag`, `build`, `package` | Linux steps add a second directory (ADR 0017) |
| Pre-sign check | Worktree `crosstalk.release-chain`, commit 7c6fffe, Sign step (`matches=` line): exactly one valid identity in the temporary keychain equals `APPLE_CODESIGN_IDENTITY` | Taken alone into 0.1.3; the intermediates and diagnostics from 7c6fffe and 15b8cce are dropped (gen 3 ruling, rc re-run succeeded with the right .p12) |
| ADR 0015 | Line 23 "A required reviewer is standard, so no write collaborator reaches signing unapproved"; operator list step 7 (line 62) negative check "as a write collaborator"; line 44 "Linux binaries are not built" | The repo has no write collaborator; both lines change (slice C2); line 44 points to ADR 0017 |
| Setup | `docs/setup.md` 8 steps, macOS only, PATH via `~/.zprofile`/`~/.zshenv` | Shrinks to download, verify, `./crosstalk install`; gets a Linux section |
| CI | `.github/workflows/ci.yml`: build, `go test -race`, vet, gofmt on `ubuntu-latest` and `macos-latest` | Every new test runs on both |
| Windows | `GOOS=windows go build ./...` fails only at `registry.go:82` (VERIFIED-LIVE); with that line stubbed in a scratch copy, the build passes and the only other compile error is `internal/peercred/peercred_test.go:72` (`unix.Socketpair`) | Section 7 |

Decision records read: ADR 0008, 0013, 0014, 0015 and the gen 3 rulings. Canonical pipeline (AGENTS.md): inbound flows through one receiver chain; install touches no message path. Harness branching outside adapters is a defect, so per-harness install code goes in the adapters.

## 3. (a) crosstalk install, uninstall, reinstall

### 3.1 Behaviour

```
crosstalk install   [claude|codex] [--yes] [--path] [--bin-dir DIR]
crosstalk uninstall [claude|codex] [--yes]
crosstalk reinstall [claude|codex] [--yes] [--path] [--bin-dir DIR]
```

No harness name: act on each of `claude` and `codex` found by `exec.LookPath`; say which were not found. Steps, in order, each printed with what it will change and confirmed `[y/N]` on a terminal:

1. **Binary.** Copy `os.Executable()` (symlinks resolved) to `<bin-dir>/crosstalk`, default `~/.local/bin`, mode 0755, by temp file in the same directory, fsync, rename. Skip when source and target are the same file. A `go install` binary (in `$GOBIN` or `~/go/bin`) is copied like any other; install says the `go install` copy is left in place. Rename, never write in place: on Linux an in-place write to a running binary fails (`ETXTBSY`), and on macOS changing the bytes of a running signed binary gets that process killed. A running shim keeps its old inode.
2. **Plugin** (per harness, Q1). Claude: `claude plugin marketplace add GoodOlClint/CrossTalk` when absent, then `claude plugin install crosstalk@crosstalk`, or `claude plugin marketplace update crosstalk && claude plugin update crosstalk@crosstalk` when installed. Codex: `codex plugin marketplace add GoodOlClint/CrossTalk` when absent, `codex plugin marketplace upgrade`, then `codex plugin add crosstalk@crosstalk`. The plugin is updated before the MCP server is registered, so a 0.1.2 plugin that still carries a server is never live next to the new entry.
3. **MCP server, absolute path.** Claude: when `claude mcp get crosstalk` shows a user-scope entry, read its env from `~/.claude.json` (read only), `claude mcp remove --scope user crosstalk`, then `claude mcp add --scope user [-e K=V ...] crosstalk -- <bin-dir>/crosstalk mcp`. Codex: in `~/.codex/config.toml`, when no `[mcp_servers.crosstalk]` table exists, append one with `command = "<abs>"`, `args = ["mcp"]`, `default_tools_approval_mode = "approve"`; when it exists, set its `command` to the absolute path and its `args` to `["mcp"]`, and keep every other key (approval mode, env) and every `[mcp_servers.crosstalk.*]` sub-table (R1 of the agent-plugins review: an approval restriction must survive). An existing entry whose command is a wrapper (`/usr/bin/env crosstalk mcp`, a shell script) is shown to the user before it is replaced, since its args are not kept. A config that declares the server any other way (inline table, dotted keys) is refused with the manual text, not edited. Backup `config.toml.crosstalk-backup` first; after writing, `codex mcp list` must succeed and `codex mcp get crosstalk --json` must show the new command, else the backup is restored.
4. **Allow rule and rule port** (Claude). Claude reads permissions from several sources: user `~/.claude/settings.json`, each project's `.claude/settings.json` (checked in) and `.claude/settings.local.json`, and managed settings. A deny or ask on the old prefix in any of them stops matching once the tools are renamed, while the new user allow grants the renamed tool, so a restriction would silently become an allow. Before any MCP or allow change, install scans every source it can find: the user file, the managed settings file for the OS, and both project files of every project directory listed in `~/.claude.json` `projects`. Old-prefix rules in the user file and in `settings.local.json` files are rewritten to `mcp__crosstalk__` (each shown and confirmed). Old-prefix deny or ask rules in a checked-in project file or a managed file are not edited: install lists each file and rule and stops before step 3, exit 1, until the user ports them by hand or passes `--skip-rule-port` after reading the list. Then add `mcp__crosstalk__*` to `permissions.allow` in the user file. JSON round-trip with stdlib, backup first, atomic write. Key order in a rewritten file may change (`encoding/json` sorts map keys); the backup keeps the original. Projects not listed in `~/.claude.json` are out of reach; setup.md says to grep for the old prefix.
5. **PATH** (optional, `--path` or a `y` answer; default no). Append one marked line (`export PATH="<bin-dir>:$PATH" # crosstalk`) to `~/.zprofile` and `~/.zshenv` on macOS, `~/.profile` and `~/.bashrc` on Linux, only when the bin dir is not already on `$PATH` in a login shell (`$SHELL -lc 'command -v crosstalk'`). Only needed for typing `crosstalk send`; the harnesses use the absolute path.
6. **Checks** (section 3.2), then the restart notice: "Running Claude and Codex sessions keep their old CrossTalk server. Restart them (in Claude, `/mcp` and reconnect also works)."

`--yes` runs steps 1-4 and 6 without asking, and step 5 only with `--path`. No terminal and no `--yes`: print the steps, change nothing, exit 2. Uninstall runs the reverse: remove the PATH lines it marked, remove `mcp__crosstalk__*` from allow (other rules the user wrote stay), remove the MCP entry (Claude `claude mcp remove --scope user crosstalk`; Codex the `[mcp_servers.crosstalk]` table with all its sub-tables, then `codex mcp list` must succeed), `claude plugin uninstall crosstalk` and `codex plugin remove crosstalk@crosstalk` (marketplaces stay; Q4), and last delete `<bin-dir>/crosstalk` only when no harness registration still names it: `crosstalk uninstall claude` with Codex still installed keeps the binary and says so. The state dir `~/.local/state/crosstalk` stays (Q5). Reinstall = install with every step applied even when its check already passes (Q3).

Exit codes: 0 all checks pass; 1 a step or check failed; 2 usage or refused (no terminal without `--yes`).

### 3.2 Post-install checks

Run at the end of install and reinstall, and alone with `crosstalk install --check`.

| # | Check | How | Failure means |
|---|---|---|---|
| K1 | Plugin at the binary's version | `claude plugin list --json`; Codex: the installed manifest under the Codex plugin cache (path to confirm in slice A5; UNKNOWN whether `codex plugin list` reports installed versions) | Old plugin; on 0.1.2 it still carries a server |
| K2 | Exactly one `crosstalk` server per harness, none from the plugin | Claude `claude mcp list` shows `crosstalk` and no `plugin:crosstalk:crosstalk`; also no `crosstalk` entry in local or project scope of `~/.claude.json`; installed plugin manifests have no `mcpServers` | Two shims, `agent_held` |
| K3 | Codex config parses, no orphan tools table | `codex mcp list` exit 0; no `[mcp_servers.crosstalk.tools.*]` header without `[mcp_servers.crosstalk]` | Codex refuses the whole config (`invalid transport`) |
| K4 | Allow rule present; no restriction left on the old prefix | User `settings.json` `permissions.allow` has `mcp__crosstalk__*`; no allow, ask or deny entry in any source scanned by step 4 names `mcp__plugin_crosstalk_crosstalk__` (unless `--skip-rule-port`, then warn per rule) | A restriction the user wrote no longer applies, or every call prompts |
| K5 | Registered command is the installed binary | Claude `claude mcp get crosstalk`; Codex `codex mcp get crosstalk --json` `.transport.command` and `.args`; command equals `<bin-dir>/crosstalk` and is executable, args equal `["mcp"]` | Harness starts a different or missing binary |
| K6 | Live MCP handshake | Start exactly the registered command, args and env as the harness would (read back from the harness, nothing appended), MCP `initialize` and `tools/list` over stdio with the go-sdk client (already a dependency), expect the seven tool names and `serverInfo.version == mcpserver.Version`, then close stdin. No `tools/call`, so no agent registers (to confirm in slice A6: `tools/list` must not create a descriptor) | Binary does not start or is a different version |
| K7 | PATH for `crosstalk send` | `$SHELL -lc 'command -v crosstalk'` | Warning only; harnesses do not need it |

### 3.3 Code shape

`internal/harness/harness.go` gains:

```go
type Installer interface {
    Found() bool                                     // harness CLI on PATH
    InstallSteps(env InstallEnv) []Step
    UninstallSteps(env InstallEnv) []Step
    Checks(ctx context.Context, env InstallEnv) []CheckResult
}
type InstallEnv struct { Bin, Version, Home string; Run func(ctx context.Context, name string, args ...string) ([]byte, error) }
type Step struct { Title, Change string; Apply func(ctx context.Context) error }
type CheckResult struct { ID, Detail string; OK, WarnOnly bool }
```

`Run` is the seam for tests: they pass a recorder or put fake `claude` and `codex` scripts on a temp `PATH`. `cmd/crosstalk/install.go` owns flags, harness selection, prompting, the binary and PATH steps, and printing; `internal/harness/claude/install.go` and `internal/harness/codex/install.go` own the rest. `internal/shim/arch_test.go` keeps passing (no shared package imports a harness). Eases Windows later: the binary copy and PATH step are the only OS-shaped parts, both in `cmd/crosstalk/install.go`.

### 3.4 Slices

| # | Slice | Files | Test: fails before, passes after | DoD |
|---|---|---|---|---|
| A1 | `Installer` interface and subcommand skeleton: flags, harness selection, refusal without terminal | `internal/harness/harness.go`, `cmd/crosstalk/main.go`, `cmd/crosstalk/install.go`, `cmd/crosstalk/install_test.go`; ADR 0008 text | `TestInstallSelectsHarnesses`: with fake `claude` only on PATH, `install --yes` acts on claude only and names codex as not found; `install codex` with codex absent exits 1. `TestInstallRefusesWithoutTerminal`: stdin not a terminal, no `--yes`: exit 2, temp HOME byte-identical before and after | Both tests green on Linux and macOS CI; `go vet` clean; usage text lists the three commands |
| A2 | Binary step: atomic copy, same-file skip | `cmd/crosstalk/install.go`, `install_test.go` | `TestInstallBinaryReplacesRunningFile`: copy a small helper binary to the target, start it, run the copy step with different bytes; the started process is still alive, target bytes equal the new source, mode 0755, no temp file left. An in-place `os.WriteFile` version fails on Linux CI with `ETXTBSY`. `TestInstallBinarySameFileSkips` | Test green on both CI OSes; manual: `go install ./cmd/crosstalk && crosstalk install --yes claude` copies from `$GOBIN` |
| A3 | Plugin drops its server; prefix moves | `plugins/crosstalk/.claude-plugin/plugin.json`, `.codex-plugin/plugin.json`, delete `claude.mcp.json`, `codex.mcp.json`; `cmd/crosstalk/plugin_test.go`; 8 eval graders; plugin `README.md` | `TestPluginShipsNoServer` replaces `TestServerKeysAndCommands`: no manifest has `mcpServers`, no `*.mcp.json` in the tree, every grader names `mcp__crosstalk__`. Fails on main (manifests carry servers) | `claude plugin validate --strict plugins/crosstalk` passes; a Codex scratch `CODEX_HOME` with `codex plugin add` from a local marketplace accepts a skill-only plugin (UNKNOWN until run); `claude plugin eval` on one case still resolves the renamed grader tools (UNKNOWN until run; if eval mocks need a plugin server, record it and keep graders on a mock name) |
| A4 | Claude steps: plugin, MCP with absolute path, allow rule with prefix port | `internal/harness/claude/install.go`, `install_test.go` | `TestClaudeInstallSteps`: recorder sees `plugin install` before `mcp add --scope user crosstalk -- <abs> mcp`; an existing entry's env is carried as `-e`. `TestClaudeAllowRulePort`: a settings file with an old-prefix deny and an unrelated key keeps the deny under the new prefix, adds the allow, keeps the unrelated key, and writes a backup. `TestClaudeRulePortStopsOnProjectDeny`: a project listed in a fixture `~/.claude.json` has a checked-in `.claude/settings.json` denying `mcp__plugin_crosstalk_crosstalk__send_message`; install exits 1, names that file and rule, and no `mcp add` and no allow change is recorded; a `settings.local.json` deny in the same project is rewritten to `mcp__crosstalk__send_message` | Tests green; one manual run in a scratch `HOME` with real `claude` registers the server (`claude mcp get crosstalk` shows the absolute path) |
| A5 | Codex steps: plugin, `config.toml` table edit | `internal/harness/codex/install.go`, `install_test.go`, `testdata/config-*.toml` | `TestCodexConfigEdit` table: (1) no table → appended with `approve`; (2) table with `default_tools_approval_mode = "prompt"` and a `tools.list_agents` sub-table → only `command` changes, both kept; (3) inline-table form → refused, file unchanged; (4) orphan `tools.*` table → K3 fails; (5) `command = "/usr/bin/env"`, `args = ["crosstalk", "mcp"]` → after the edit `args` is `["mcp"]`, and K6 started from the read-back entry passes (it fails if args are kept). Fixtures copied from a real `codex plugin add`-rewritten config (scrubbed) | Tests green; manual in a scratch `CODEX_HOME`: `codex mcp list` succeeds and `codex mcp get crosstalk --json` shows the absolute path; on a forced failure the backup is restored |
| A6 | Checks K1-K7 and `--check` | `cmd/crosstalk/install.go`, both adapter `install.go`, tests | `TestCheckHandshake`: K6 against the test binary itself passes; against a fake binary that prints wrong version fails. `TestCheckOrphanToolsTable` (K3), `TestCheckTwoServers` (K2 with fake `claude mcp list` output listing both). Also: running K6 creates no file under a temp state dir | Each check has a failing fixture and a passing one; output lists K1-K7 with pass, fail or warn |
| A7 | Uninstall and reinstall | `cmd/crosstalk/install.go`, adapter `install.go`, tests | `TestUninstallReverses`: after install then uninstall in a temp HOME with fakes, the binary is gone, settings and config equal their pre-install bytes except rules the user had (old-prefix rules stay ported), state dir untouched, plugin remove commands recorded. `TestReinstallReappliesAll`. `TestScopedUninstallKeepsSharedBinary`: install both with fakes, `uninstall claude`; the binary stays, Codex's registered command still runs K6 green; then `uninstall codex` deletes it | Tests green; manual round trip on this Mac in a scratch HOME |
| A8 | PATH step | `cmd/crosstalk/install.go`, test | `TestPathStepMarkedAndIdempotent`: two installs add one marked line per file; uninstall removes exactly it | Test green on both OSes |
| A9 | Docs | `docs/setup.md`, `README.md`, `plugins/crosstalk/README.md`, ADR 0013 (in place), `AGENTS.md` architecture line, design §9 if it names the install | `grep -c 'crosstalk install' docs/setup.md` ≥ 1 and `grep -c 'mcp__plugin_crosstalk' docs/setup.md` = 0 (fails before) | A clean-account run (scratch macOS account) follows setup.md alone: download, verify, `./crosstalk install`, try-it step passes; gaps go back into setup.md |

Size: A1 S, A2 S, A3 S (plus the two UNKNOWN runs), A4 M, A5 M, A6 M, A7 M, A8 S, A9 M with the clean run. About 4 working days.

## 4. (b) Linux

Builds: `linux/amd64` and `linux/arm64`. arm64 is in because it costs one `go build`; it is never executed before release (no arm64 Linux host in the plan).

| # | Slice | Files | Test: fails before, passes after | DoD |
|---|---|---|---|---|
| B1 | `build-linux` and `package-linux` in the release script | `scripts/release-build.sh`, `cmd/crosstalk/release_test.go` | `TestReleaseBuildLinux`: runs `scripts/release-build.sh build-linux $tmp` and `package-linux $tmp v<Version>`; both tarballs exist, each holds exactly `crosstalk`, mode 0755, `debug/elf` reads `EM_X86_64` and `EM_AARCH64`, each `.sha256` verifies with `shasum -a 256 -c`. Fails before (unknown subcommand) | Green on Linux and macOS CI; `shellcheck` clean |
| B2 | Release job builds, packages and publishes Linux assets outside the signing path | `.github/workflows/release.yml`, ADR 0015 line 44, ADR 0017 | Static: `actionlint` clean; a grep check that `dist-linux` appears in no step that runs `codesign`, `notarytool`, `security` or `lipo`; publish `files=(...)` lists the four Linux files | Next rc tag run (operator yes needed) publishes 2 macOS + 4 Linux files; downloading the amd64 tarball on a Linux host and `shasum -c` prints OK |
| B3 | Install on Linux | `cmd/crosstalk/install.go` (PATH files), tests already in A2 and A8 run on `ubuntu-latest` | A8 test asserts Linux writes `~/.profile` and `~/.bashrc`, macOS `~/.zprofile` and `~/.zshenv` | Green on both CI OSes |
| B4 | setup.md Linux section | `docs/setup.md` | grep: section heading `Linux` exists and names `linux-amd64.tar.gz` (fails before) | Covers the report's mismatches: tarball and checksum (no `brew`, no Go), `~/.local/bin` on PATH via Debian `~/.profile`, Codex needs the full `codex-package-*` tarball (`codex-code-mode-host`, `app-server daemon start`), `model_reasoning_effort = "low"` in `config.toml` as the lowest the default model takes, the bubblewrap warning, and that any `-c` override starts an embedded Codex server (C3). F1 is stated as unmeasured until B5 |
| B5 | Planned llm VM run (not executed by this plan) | Run sheet below; report to the coordinator archive (private) | Pass criteria below | Report with raw logs; setup.md Linux section corrected from it |

### 4.1 B5 run sheet (planned, not executed)

Preconditions: 0.1.3 implemented through A9 and B4; an rc tag published (operator yes), so the run tests the real asset. Fallback before a tag: cross-build on the Mac as on 2026-11-05.

1. Tell homelab coordinator t1p before starting, with the planned window.
2. Everything within the limits file in the coordinator archive (one throwaway user, home only, no package installs, memory floor, ports, logouts before removal); this plan copies none of its detail.
3. As the throwaway user: download the `linux-amd64` tarball and `.sha256`, `shasum -a 256 -c`, extract, `./crosstalk install --yes --path`. Record K1-K7.
4. Operator does the Claude and Codex logins.
5. Codex config: `model_reasoning_effort = "low"` in `~/.codex/config.toml`. Start plain `codex` with no `-c` and no `--remote`.
6. Measure F1: does plain `codex` attach to the managed daemon (`list_agents` row `reachable: true`, non-empty `cwd`)? Record either way. If not, start `codex app-server daemon start` and retry once, recording which made it reachable.
7. Repeat run tests (a) discovery, (c) Claude to Codex both ways, (e) `crosstalk send`. F3 (Codex shim outliving its TUI) is observed and logged, not chased.
8. `crosstalk uninstall --yes`; record that binary, MCP entries, allow rule and PATH lines are gone and the state dir remains.
9. Logouts, remove the user, prove removal as in the 2026-11-05 report. Tell t1p it finished.

Pass: K1-K7 pass on install (K7 may warn), (a) (c) (e) pass with plain `codex` or the daemon outcome is recorded as F1 data, uninstall leaves nothing but the state dir, VM services unchanged.

Size: B1 S, B2 S, B3 S, B4 S, B5 M (operator time for logins). About 1.5 days.

## 5. (c) Small items

| # | Slice | Files | Test: fails before, passes after | DoD |
|---|---|---|---|---|
| C1 | Pre-sign identity check from release-chain 7c6fffe: in the Sign step, before `codesign`, count valid identities in the temporary keychain whose name equals `APPLE_CODESIGN_IDENTITY`; anything but 1 fails with one error line. Also `set -euo pipefail` in the import and sign steps. Not taken: the two pinned intermediates and the `identities` diagnostics | `.github/workflows/release.yml`, `scripts/release-build.sh` (new `check-identity KEYCHAIN`, holding the awk matcher so it is testable), `cmd/crosstalk/release_test.go` | `TestCheckIdentity`: a fake `security` on PATH prints 0, 1 and 2 matching identities (and one near-miss name); exit nonzero, zero, nonzero, nonzero. Fails before (no subcommand) | `actionlint` and `shellcheck` clean; the next rc run passes the check; release-chain worktree removed with `wt remove` after merge (coordinator) |
| C2 | ADR 0015 operator list: drop the write-collaborator negative check (step 7, line 62) and reword line 23 to "The operator is the required reviewer, so signing never runs unapproved" | `docs/decisions/0015-signed-macos-release-in-ci.md` | `grep -c 'write collaborator' docs/decisions/0015-*.md` = 0 (fails before: 2) | Steps renumber 1-7; Rehearse becomes step 7 |
| C3 | setup.md troubleshooting row | `docs/setup.md` | grep for the row's `-c` text (fails before) | Row: "A Codex row with empty `cwd`, `reachable: false` after starting `codex -c ...`" → "Any `-c` override makes Codex run its own embedded app server instead of the managed daemon, on every platform, so CrossTalk cannot deliver to it. Put the setting in `~/.codex/config.toml` and start `codex` without `-c`." |
| C4 | F6 issue text, drafted here, not filed | none | none (text only) | Text below; the coordinator files it after operator yes |

C4 draft issue:

> **Title:** Codex `list_agents` name changes to the thread title once the thread is named
>
> **Observed** (Linux, codex-cli 0.166.0, CrossTalk 0.1.2, 2026-11-05): a Codex session listed as `codex-cx-6d99e5c7` was later listed under the thread title Codex generated ("Call CrossTalk list_agents"), and its replies were labelled with that title. A peer that saved the first name can no longer address it by that name.
>
> **Expected:** a stable name for the life of the session, or a documented rule for when it changes.
>
> **Where:** Codex discovery takes `Name` from the loaded thread (`internal/harness/codex/harness.go:93`, `t.Name`). Options: keep the first name the shim registered for that thread; or prefer the descriptor name over the thread title; or document the change and resolve old names in `send_message`.
>
> **Also seen:** after `codex app-server daemon start`, one nameless `crosstalk: false` Codex row in the same cwd.

Size: C1 S, C2 S, C3 S, C4 S. Under half a day.

## 6. Sequencing and release

Order: C2, C3 (docs, no risk) → A1, A2, A3 → A4, A5 → A6, A7, A8 → B1, B2, B3, C1 → A9, B4 → version bump to 0.1.3 (manifests and `mcpserver.Version` under the existing test) → rc tag (operator yes) → B5 and the A9 clean-account run → `v0.1.3` (operator yes). Publication boundary: both marketplace files point at `./plugins/crosstalk` on `main`, unpinned, so whatever is on `main` is what a fresh `plugin install` gets, version bump or not. A3 (server removed from the plugin) therefore never lands on `main` alone: A3, A4, A5, A6, A7, A9 and the version bump merge to `main` as one PR (stacked commits, each with its test), right before the rc tag. Existing 0.1.2 users who update the plugin after that merge lose the plugin's server until they run the new binary's `crosstalk install`; the release notes and the plugin README say so first, and Q11 asks whether to also pin the marketplace source to a release tag.

Each slice is one PR-sized commit with its test; the Codex adversarial review runs before each PR (CLAUDE.md). Total: about 6 working days plus operator time for the rc tag, B5 logins and the clean-account run.

## 7. (d) Windows map

Measured today: `GOOS=windows GOARCH=amd64 go build ./...` fails only at `internal/registry/registry.go:82` (`unix.Flock`, `LOCK_EX`, `LOCK_NB`, `EWOULDBLOCK` at 84). With those lines stubbed in a scratch copy, `go build ./...` passes, and `go vet` and `go test -c` fail only at `internal/peercred/peercred_test.go:72` (`unix.Socketpair`). Everything below that is not a compile error is a runtime blocker, read from the source; none of it has run on Windows.

| # | Blocker | file:line | Fix shape | Size |
|---|---|---|---|---|
| W1 | Name and agent locks use `flock` | `internal/registry/registry.go:82-88` | `lock_unix.go` / `lock_windows.go` with `windows.LockFileEx` (`LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY`; `golang.org/x/sys/windows` is in the module already) | S |
| W2 | Test uses `unix.Socketpair` | `internal/peercred/peercred_test.go:72` | `//go:build !windows` on that test | S |
| W3 | Native sender identity: peer pid of a unix-socket connection | `internal/peercred/peercred_other.go:5` returns `ErrUnsupported`, so every native Claude sender is unverified and its no-auth frame is closed (design §5 shape (b)) | `SIO_AF_UNIX_GETPEERPID` through `WSAIoctl` on the socket handle (UNKNOWN: Windows build that has it, and Go's `syscall.RawConn` access) | M |
| W4 | Process start time for liveness and identity | `internal/procstart/procstart_other.go:11` errors; used by `registry.New` (`registry.go:37`) and Claude `alive` (`claude.go:96-99`), so every session and descriptor looks dead | `procstart_windows.go`: `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)` + `GetProcessTimes` | S |
| W5 | Key and token files must be mode 0600 | `internal/udsingress/udsingress.go:98`, `internal/wire/wire.go:281`, `internal/a2a/server.go:489`: Windows reports permission bits from the read-only flag only, so the check always fails | Per-OS check: on Windows verify the file's DACL grants only the current user (or rely on the profile directory ACL and say so in an ADR). Security boundary: needs its own decision | M |
| W6 | Socket mode and path | `udsingress.go:134-138` (`ListenUnix`, `Chmod 0600`); `claude/harness.go:139` (`ModeSocket` test) | AF_UNIX works on Windows 10 1803+ in Go; `Chmod` is a no-op, so access rests on the directory ACL (W5). Whether `os.Stat` reports `ModeSocket` for an AF_UNIX file is UNKNOWN | S, plus W5 |
| W7 | Claude Code on Windows: session registry and messaging socket | `claude/harness.go:70-90` reads `~/.claude/sessions/<ppid>.json` and dials `MessagingSocketPath` (`claude.go:292`, `"unix"`) | UNKNOWN whether Claude Code on Windows writes `sessions/<pid>.json`, has a per-session messaging socket, and which transport (AF_UNIX or named pipe). If named pipe: a new dialer and a new ingress protocol server (ADR 0008 allows one `NativeIngress`) | M if AF_UNIX, L if named pipe |
| W8 | Parent pid identity | `mcpserver/server.go:91`, `shim/shim.go:123`, `claude.go:129` (`os.Getppid`) | Go implements `Getppid` on Windows (toolhelp snapshot). UNKNOWN whether Claude Code on Windows starts the MCP server directly or through `cmd.exe` / a `.cmd` shim, which would make the parent the wrapper | S if direct, M if wrapped |
| W9 | Codex managed daemon and control socket | `codex/appserver/client.go:21` (`~/.codex/app-server-control/app-server-control.sock`), `client.go:133` (`DialContext("unix")`) | UNKNOWN whether Codex on Windows runs `app-server daemon` and on what transport. Without it, Codex sessions can send but never receive (same as `--no-daemon` today) | M if AF_UNIX, L if named pipe or absent |
| W10 | Atomic replace of state files | `registry.go:346`, `journal/journal.go:379` (`os.Rename` over an existing file) | Go's `os.Rename` on Windows uses `MoveFileEx(REPLACE_EXISTING)`, which fails while another process holds the target open without share-delete. Measure on the VM; fix by retry or by keeping the files closed between writes | M |
| W11 | Signals | `cmd/crosstalk/main.go:105` (`os.Interrupt, syscall.SIGTERM`) | Compiles; on Windows only Ctrl-C/Ctrl-Break arrive. The shim already ends on stdin EOF from the MCP transport. Verify, likely no change | S |
| W12 | Paths | `registry.go:40-46` (`~/.local/state/crosstalk`), `claude.go:77-83`, `client.go:36-43` | Works under `%USERPROFILE%`, but the native place is `%LOCALAPPDATA%\crosstalk`; AF_UNIX path limit is 108 bytes (`C:\Users\<name>\.local\state\crosstalk\sock\<16 hex>.sock` fits for normal names). Name lock files: `url.PathEscape` does not escape Windows reserved names (`CON`, `NUL`) or case collisions (`registry.go:59-64`) | S |
| W13 | Install on Windows | `cmd/crosstalk/install.go` (0.1.3) | Bin dir `%LOCALAPPDATA%\Programs\crosstalk\crosstalk.exe`; PATH through `HKCU\Environment` plus `WM_SETTINGCHANGE`; a running `.exe` cannot be replaced by rename-over, so rename the old file aside first and delete it on the next run; uninstall cannot delete its own running `.exe` | M |
| W14 | Release asset | `scripts/release-build.sh`, `release.yml` | `windows/amd64` and `arm64` zips are a cross-build like Linux (S). Unsigned binaries meet SmartScreen; Authenticode signing needs a certificate the operator does not have (decision, cost) | S unsigned, L signed |
| W15 | CI | `.github/workflows/ci.yml` | Add `windows-latest` to the matrix. Many tests assume `/tmp`-style paths, unix sockets and 0600 modes; expect a pass of test fixes | M to L |

Total: compile-clean is S (W1, W2: about 1 hour). A working Windows release is about 6-9 working days if Claude Code and Codex use AF_UNIX sockets and a session registry on Windows, and 10-15 or more if either uses named pipes, has no registry or has no Codex daemon; plus signing if wanted. The deciding facts (W7, W8, W9) are unknown and need the Windows VM.

**Recommendation: separate phase, after 0.1.3, with its own brownfield gate.** Reasons: the size is several times all of 0.1.3; the deciding facts are unknown until a 1-2 hour probe on a Windows VM (does Claude Code write `~/.claude/sessions/<pid>.json` and a messaging socket, does Codex run a daemon, what transport each uses); W3, W5 and W7 touch the transport identity and trust model (ADR 0004), which is an architectural decision, not a port. W1 and W2 could be folded in as a compile-clean commit with a `GOOS=windows go build` and `go vet` CI step (S), which only stops new unix-only code from landing; offered as Q8. Since this plan recommends a separate phase, it plans no Windows slices.

## 8. Open questions for the operator

Each has options and a recommendation. Settled by rulings and not asked again: install exists with optional harness name; binary to a stable place; MCP at the absolute path; settings; optional PATH; prompt per step; checks; uninstall deletes the binary; Linux amd64 in 0.1.3; Windows not implemented here.

- **Q1. Plugin step: does `crosstalk install` run the harness plugin commands, or is it README-first?** (a) Install runs `claude plugin install` and `codex plugin add` (recommended: the 2026-11-05 ruling says install "registers the plugins", and the plugin must be updated before the MCP entry is added or a 0.1.2 plugin gives two shims); (b) README-first: the plugin README says "install this plugin, then run `crosstalk install`", install only does binary, MCP, settings and checks, and K1/K2 catch an old plugin. The kickoff's default was (b); this plan takes (a) and keeps (b)'s text in the plugin README for users who install the plugin first.
- **Q2. Bin dir.** (a) `~/.local/bin` with `--bin-dir` to override (recommended); (b) fixed `~/.local/bin`; (c) `/opt/homebrew/bin` when writable. (c) makes an admin and a non-admin account behave differently.
- **Q3. Reinstall meaning.** (a) Install with every step re-applied even when its check passes, no uninstall first (recommended: keeps user rules and Codex approval mode); (b) uninstall then install (drops a user's Codex `prompt` mode unless carried).
- **Q4. Uninstall and marketplaces.** (a) Remove the plugin, leave the marketplaces added (recommended: other plugins may use a marketplace, and re-adding is one command); (b) remove the `crosstalk` marketplaces too.
- **Q5. Uninstall and state.** (a) Keep `~/.local/state/crosstalk` (journals, tokens) (recommended: data-loss guard); (b) `--purge` flag deletes it; (c) always delete.
- **Q6. Settings write on Claude.** (a) Stdlib JSON round-trip with a backup; key order in `settings.json` may change (recommended); (b) an order-preserving JSON edit (more code); (c) do not write, print the rule for the user to add.
- **Q7. Linux arm64.** (a) Build and ship it unexecuted, setup.md says so (recommended: one `go build`); (b) amd64 only until an arm64 host can run it.
- **Q8. Windows.** (a) Separate phase after 0.1.3 with its own gate, starting with a 1-2 hour probe on the Windows 11 VM (recommended); (b) fold only W1 and W2 plus a Windows compile CI step into 0.1.3 (S); (c) fold the whole map into 0.1.3. Also: which Windows 11 VM, and who does its logins.
- **Q9. Claude MCP scope.** (a) `--scope user` (recommended: one entry for every project, matching the old plugin); (b) let `--scope` be chosen. A project-scope `crosstalk` entry would shadow it; K2 warns.
- **Q11. Marketplace pin.** The marketplaces serve `main`, so a 0.1.2 user who updates the plugin after 0.1.3 merges gets a skill-only plugin and no server until they install the 0.1.3 binary and run `crosstalk install`. (a) One PR at the release boundary plus release notes and README (recommended now; no new mechanism); (b) also pin the marketplace plugin source to a release tag, if both marketplace formats support a git ref (Claude's does per its docs; Codex UNKNOWN), so plugin updates move only at a release; (c) keep a plugin-supplied server one more release for old binaries (two shims for anyone who also runs install: rejected by ADR 0016).
- **Q10. B5 timing.** (a) After an rc tag, on the real Linux asset (recommended); (b) before any tag, on a Mac cross-build.

## 9. Review disposition

Codex adversarial review, 2026-11-05, on c453d5e (branch diff against main; log `/tmp/crosstalk-0.1.3-codex.log`). Verdict: needs-attention. Codex's summary: "Do not approve implementation yet. The plan can bypass existing permissions, break the unselected harness, and report success for a broken registration." Codex read the plan, ADRs and cited code; it did not run live migrations, release builds or Windows probes. Severity is Codex's. 4 findings: 4 fixed, 0 rejected.

| # | Sev | Finding, as Codex stated it | Disposition |
|---|---|---|---|
| R1 | high | "Only ~/.claude/settings.json is rewritten. A project deny for mcp__plugin_crosstalk_crosstalk__send_message stops matching after the rename, while the new user-wide allow grants mcp__crosstalk__send_message. K4 and the A4 fixture miss this bypass." | **Fixed in eb228c3.** Step 4 scans user, managed, and project and local settings of every project in `~/.claude.json`; rewrites user and local files; a checked-in or managed old-prefix deny or ask stops install before registration. K4 covers every scanned source; A4 adds `TestClaudeRulePortStopsOnProjectDeny`. Gap stated: projects not listed in `~/.claude.json` are not found |
| R2 | high | "After installing both harnesses, uninstall claude removes only Claude's registration but unconditionally deletes the shared <bin-dir>/crosstalk. Codex retains an absolute command pointing to a missing executable." | **Fixed in eb228c3.** Uninstall deletes the binary only when no registration names it; A7 adds `TestScopedUninstallKeepsSharedBinary`; ADR 0016 says so |
| R3 | high | "Both marketplace files reference ./plugins/crosstalk directly … A fresh marketplace install during that interval therefore receives a skill-only plugin labelled 0.1.2 and no server. The claim that users cannot see this until the version bump is false for fresh installs." | **Fixed in eb228c3.** Section 6: A3 never lands on `main` alone; A3-A7, A9 and the bump merge as one PR at the release boundary. The remaining effect on 0.1.2 users who update the plugin is stated in ADR 0016 and asked as Q11 (pin the marketplace to a tag) |
| R4 | medium | "Step 3 preserves Codex args while replacing only command, but K6 always supplies mcp … migration produces <installed-binary> crosstalk mcp … yet parsing, command-path validation and K6 all pass." | **Fixed in eb228c3.** Step 3 sets `args = ["mcp"]` and shows a wrapper entry before replacing it; K5 checks args; K6 starts the read-back command, args and env with nothing appended; A5 fixture (5) pins it |
