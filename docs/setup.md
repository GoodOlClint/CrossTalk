# Set up CrossTalk on macOS or Linux

CrossTalk lets Claude Code and Codex sessions on one machine message each other by name. You need macOS or Linux, and Claude Code and/or Codex. The steps below are for macOS; [Linux](#linux) lists what differs. A first clean-account run (2026-11-04) passed the basic plumbing and the Codex authority case; both cross-harness answer round trips passed live with the plugin only, and Codex started outside a shell passed on result. A Claude peer read a file on a soft ask on that run. In a live replica at c4025a9 (typed and bound Claude session, real Codex asker, harmless question answered first), Claude did not read the file before its user approved in 3 of 3 Haiku runs and 1 of 1 Sonnet run, where the older text read in 1 of 1; the sample is small and the rule is advice to the model, not enforcement, so a session in auto mode could still read on a peer's word. An explicit `prompt` approval entry is indirect only ([open facts](decisions/0013-agent-plugins-and-skill-packaging.md#not-yet-run-live)). `crosstalk install` (0.1.3, [ADR 0016](decisions/0016-crosstalk-install-registers-mcp-at-the-installed-path.md)) has run against the real `claude` and `codex` CLIs in scratch home directories, and on a clean account on 2026-11-05 with v0.1.3-rc.1, where it passed.

1. **Download and verify the binary.** The release asset for macOS is a universal binary (one file that runs on Apple silicon and Intel Macs) that is signed and notarized, so you do not need Go. Replace `v0.1.3` with the release you want. Run this in a scratch directory:
   ```sh
   TAG=v0.1.3
   curl -fLO "https://github.com/GoodOlClint/CrossTalk/releases/download/$TAG/crosstalk-$TAG-macos-universal.zip" &&
   curl -fLO "https://github.com/GoodOlClint/CrossTalk/releases/download/$TAG/crosstalk-$TAG-macos-universal.zip.sha256" &&
   shasum -a 256 -c "crosstalk-$TAG-macos-universal.zip.sha256" &&
   unzip "crosstalk-$TAG-macos-universal.zip" &&
   ./crosstalk version
   ```
   The chain stops at the first failure, so a failed download or checksum leaves nothing to run; the checksum line must print `OK`.

   Terms: to *notarize* is to upload a signed program to Apple, which scans it and records a ticket that macOS checks online. A *hardened runtime* binary is signed with extra OS-enforced limits on what it can do. *Quarantine* is the flag (`com.apple.quarantine`) that browsers and some other apps put on a downloaded file so macOS runs its Gatekeeper check on first launch. To *staple* is to attach the notarization ticket to the file so it works offline.

   First run on macOS: the binary is signed with a Developer ID certificate and its zip is notarized, but a bare command-line binary cannot be stapled, so macOS finds the ticket online. `curl` does not set the quarantine flag, so the download above skips the Gatekeeper prompt (this is how macOS behaves for curl downloads; not re-checked on a release build). If you download the zip with a browser instead, the extracted file may carry the quarantine flag; whether Gatekeeper then opens it silently or shows a prompt for a notarized bare binary is unverified. If it blocks, `xattr -d com.apple.quarantine crosstalk` clears the flag. `spctl --assess` on a bare binary reports "rejected" even when it is signed, because it judges apps; do not use it as the check, use `codesign --verify --strict --verbose=2 crosstalk`.

   Alternative, build from source with Go. The repo is public, so `go` needs no `GOPRIVATE`, no `gh auth` and no GitHub login: `GOBIN="$PWD" go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@latest`. `@latest` resolves to the newest release tag.
2. **Install.** Run `./crosstalk install`. It finds `claude` and `codex` on the PATH (the list of directories your shell searches for commands) and sets up each one it finds. On a fresh Mac with Homebrew, `claude` and `codex` are not on the PATH until you add Homebrew's `shellenv` line, `eval "$(/opt/homebrew/bin/brew shellenv)"` (`brew shellenv` prints the PATH settings Homebrew needs), to `~/.zprofile` (the file zsh reads at login) and open a new terminal; do that first. (`./crosstalk install claude` or `./crosstalk install codex` picks one). Each step prints what it will change and asks `[y/N]`; `--yes` answers yes to every required step. The steps:
   - copy the binary to `~/.local/bin/crosstalk` (`--bin-dir DIR` picks another directory); a running CrossTalk server keeps its old file;
   - install or update the plugin, which carries the skill: `claude plugin install crosstalk@crosstalk`, `codex plugin add crosstalk@crosstalk`, each from the `GoodOlClint/CrossTalk` marketplace, which pins the plugin to the last release tag;
   - Claude: port every allow, ask or deny rule that names a 0.1.2 tool name (the plugin-server prefix, ending in `crosstalk_crosstalk__`) to the new prefix `mcp__crosstalk__` in `~/.claude/settings.json` and in each project's `.claude/settings.local.json`; a deny or ask on the old prefix in a checked-in `.claude/settings.json` or in managed settings stops the install, naming the file and rule, until you port it by hand (or pass `--skip-rule-port` after reading the list);
   - register the MCP server at the installed path: Claude `claude mcp add --scope user crosstalk -- ~/.local/bin/crosstalk mcp` (an existing user entry is replaced and keeps its `env`); Codex a `[mcp_servers.crosstalk]` table in `~/.codex/config.toml` with that `command`, `args = ["mcp"]` and `default_tools_approval_mode = "approve"` (an existing table keeps its approval mode, `env` and `tools` sub-tables; an entry that wraps the command, such as `/usr/bin/env crosstalk mcp`, is shown before its args are replaced);
   - Claude: add `mcp__crosstalk__*` to `permissions.allow` in `~/.claude/settings.json`;
   - optional, `--path` or a `y` answer: add `~/.local/bin` to the PATH in `~/.zprofile` and `~/.zshenv`, only needed to type `crosstalk send`. The harnesses use the absolute path, so a harness started outside a shell finds the server.

   Every changed file is backed up next to itself as `<file>.crosstalk-backup` first (Claude's `settings.json` is rewritten with sorted keys; the backup keeps the original). Without a terminal and without `--yes`, install prints the steps, changes nothing and exits 2. Install ends with checks K1 to K7: plugin at the binary's version (K1), exactly one CrossTalk server per harness and none from the plugin (K2), Codex accepts its config (K3), allow rule present and no rule left on the old prefix (K4), the registered command is the installed binary (K5), a live MCP handshake with it (K6), `crosstalk` on a login shell's PATH (K7, warning only). `crosstalk install --check` runs the checks alone. Exit 0 means every check passed.

   Projects that `~/.claude.json` does not list are out of install's reach: if you keep Claude settings in other directories, run `grep -rn crosstalk_crosstalk__ --include='settings*.json' ~/src` (your source root) and port what it finds to `mcp__crosstalk__`.
3. **Restart** every running Claude and Codex session. A running session keeps the old server (in Claude, `/mcp` and reconnect also works).
4. **Try it.** Open one Claude and one Codex session (or two of either). In one, say: "List the other agents, then ask the session named `<its name>` what directory it is in." Name a target from the list. A Codex session's name is the text of its first message, so name a Codex target by its `cwd` (working directory) when its name is a message. An unnamed idle Codex row is a thread that never ran a turn; ignore it. The answer arrives as a new message. `ask` needs the target to have made its first CrossTalk call; before that, use `send_message`. A Claude session nobody has typed into yet cannot answer: type one line in it first. An `ask` has one reply: if the peer refuses, that spends it, and after its user approves, a second `reply` is rejected and the answer goes by `send_message`. A new directory shows a folder-trust dialog (each harness asks once whether you trust the files in that directory); accept it. If a peer asks you before answering anything else, that is the notice working: allow it or answer for it.
5. **Upgrade.** Download the new release as in step 1 and run its `./crosstalk install` (or `./crosstalk reinstall`, which re-applies every step), then restart sessions. From 0.1.2: plugin 0.1.2 started the server itself; once the plugin updates it has none, so run the new binary's `crosstalk install`, which also ports the old-prefix rules. Moving the binary to another directory needs `crosstalk reinstall --bin-dir DIR`.
6. **Send from a shell.** `crosstalk send` posts a message from a terminal, outside any session. The receiving session sees the sender as `@unverified`, which means CrossTalk could not tie the message to a session or credential, and the model treats it as an unauthenticated peer: it tells you and asks before acting on it.
7. **Uninstall.** `crosstalk uninstall` reverses install: it removes the allow rule (rules you wrote stay, under the new prefix), the MCP entries (the Codex table with all its sub-tables), both plugins (the marketplaces stay), the PATH lines it added, and last the binary, which it keeps while another harness still runs it (`crosstalk uninstall claude` with Codex still set up keeps it). The state directory `~/.local/state/crosstalk` (journals, tokens) stays unless you pass `--purge` on a full uninstall; `--purge` refuses while a running session's CrossTalk server holds an agent there. For a full clean-up, also run `claude plugin marketplace remove crosstalk` and `codex plugin marketplace remove crosstalk`, since uninstall keeps the marketplaces. A marketplace entry left by an rc rehearsal (a trial run of a release candidate from a scratch marketplace) points at that scratch marketplace until you remove it.

`codex --no-daemon` tabs can send and pull with `receive_message` but cannot receive delivered messages: the sender gets `unreachable`. To receive in such a tab, give the tab its own app server and tell CrossTalk where it is (not run live):

```sh
S="$HOME/.codex/crosstalk-tab.sock"
codex app-server --listen "unix://$S" &
```

Add the endpoint to the `[mcp_servers.crosstalk]` table that `crosstalk install` wrote; an entry holding only `env` makes `codex mcp list` fail with "invalid transport", and a later `crosstalk install` keeps the `env`:

```toml
[mcp_servers.crosstalk]
command = "/Users/you/.local/bin/crosstalk"
args = ["mcp"]
default_tools_approval_mode = "approve"
env = { CROSSTALK_CODEX_ENDPOINTS = "unix:///Users/you/.codex/crosstalk-tab.sock" }
```

Checked in a scratch `CODEX_HOME`: `codex mcp list` accepts this entry. Not checked in a live session.

Then start the tab against that server:

```sh
codex --remote "unix://$S"
```

`CROSSTALK_CODEX_ENDPOINTS` is a comma-separated list of Codex app-server endpoints (`unix://PATH` or `ws://HOST:PORT`) that CrossTalk queries besides the managed daemon. A `ws://` endpoint carries peer text in cleartext; use it only on this machine's loopback.

## Upgrading from a plugin that bundled the server (before 0.1.3)

Before 0.1.3 the CrossTalk plugin started the server itself. Since 0.1.3 the plugin carries only the skill, and `crosstalk install` registers the server (ADR 0016). A machine that took the plugin update but not the binary has the skill and no server: Codex sessions say the CrossTalk tools are unavailable, and nothing else says why. Claude can look fine, because the old plugin's server (`mcp__plugin_crosstalk_crosstalk__` tools) keeps working until the plugin updates.

1. **Get the new binary.** Run `brew install goodolclint/tap/crosstalk`, or download the release as in step 1 above.
2. **Register the server.** Run `crosstalk install --yes`. It updates the plugin and registers the server at the binary's absolute path in Claude and in Codex.
3. **Check.** Run `crosstalk install --check` (every line must say PASS), and `codex mcp list` (it must list `crosstalk`).
4. **Restart** every running Claude and Codex session. A running session keeps the server set it started with.

A prerelease binary (`0.1.4-rc.1`) installs the last release's plugin, because the marketplace stays on the last final tag. K1 passes with a note in that case. To test the rc plugin too, set `CROSSTALK_MARKETPLACE` to the rc tag before `crosstalk install`.

## Homebrew

Homebrew is a package manager for macOS and Linux; a *formula* is its recipe for one program, and a *tap* is a repository of formulae. From release v0.1.4 on, CrossTalk has a formula in the `goodolclint/tap` tap, and it installs the same signed release binary as the download in step 1. Until v0.1.4 is released the `brew` lines below do not work.

1. **Install the binary.** Run `brew install goodolclint/tap/crosstalk`. It puts `crosstalk` on your PATH and sets up nothing in Claude or Codex.
2. **Set up the harnesses.** Run `crosstalk install` (the steps and checks are in step 2 above). It sees the Homebrew binary, copies nothing, and registers `$(brew --prefix)/bin/crosstalk` (`brew --prefix` prints Homebrew's install directory, such as `/opt/homebrew`). That path stays valid when `brew upgrade` moves to a new version.
3. **Upgrade.** Run `brew upgrade crosstalk`, then `crosstalk install` again so the plugin (which carries the skill) matches the new binary.
4. **Uninstall.** Run `crosstalk uninstall` before `brew uninstall crosstalk`: uninstall never removes a file under Homebrew's prefix, so the binary stays for Homebrew to remove, and `brew uninstall` does not undo the Claude and Codex setup.
5. **Shadowed command.** If an earlier install left `~/.local/bin/crosstalk` and that directory comes before Homebrew's in your PATH (the list of directories your shell searches), `crosstalk` runs the old copy and install registers it instead of the Homebrew binary. Check with `command -v crosstalk`; it must print `$(brew --prefix)/bin/crosstalk`. If not, run `"$(brew --prefix)/bin/crosstalk" install`, then remove the old copy with `rm ~/.local/bin/crosstalk`.
6. **Restart** every running Claude and Codex session after each of these steps. A running session keeps the old server.

## Linux

Release assets: `crosstalk-<tag>-linux-amd64.tar.gz` and `crosstalk-<tag>-linux-arm64.tar.gz`, each with a `.sha256` beside it. They are not signed (no Gatekeeper on Linux) and need no Go and no `brew`. The arm64 binary is built but has never been run before a release. Install on an x86-64 machine:

```sh
TAG=v0.1.3
curl -fLO "https://github.com/GoodOlClint/CrossTalk/releases/download/$TAG/crosstalk-$TAG-linux-amd64.tar.gz" &&
curl -fLO "https://github.com/GoodOlClint/CrossTalk/releases/download/$TAG/crosstalk-$TAG-linux-amd64.tar.gz.sha256" &&
sha256sum -c "crosstalk-$TAG-linux-amd64.tar.gz.sha256" &&
tar -xzf "crosstalk-$TAG-linux-amd64.tar.gz" &&
./crosstalk install
```

Then follow steps 3 to 7 above. What differs from macOS:

- **PATH.** `--path` adds the marked line to `~/.profile` and `~/.bashrc`. Debian's default `~/.profile` already adds `~/.local/bin` when that directory exists at login, so a new login may find `crosstalk` without `--path`. The harnesses do not need it either way.
- **Claude and Codex on the PATH.** Claude's `install.sh` puts `claude` in `~/.local/bin`, which a new login finds. The full Codex package needs its `bin` and `codex-path` directories added to the PATH in `~/.profile` (the file your login shell reads on Linux).
- **Codex package.** Install Codex from the full `codex-package-*` release tarball, which includes `codex-code-mode-host`; with the bare `codex` binary alone, `codex app-server daemon start` fails and CrossTalk cannot deliver to Codex.
- **Codex daemon.** Whether a plain `codex` started on Linux attaches to the managed app server, as it does on macOS, is not measured yet. If its `list_agents` row shows `reachable: false` or an empty `cwd`, run `codex app-server daemon start` and start `codex` again.
- **Codex settings.** Put settings in `~/.codex/config.toml`, never on the command line: any `-c` override starts Codex's own embedded app server, which CrossTalk cannot reach (troubleshooting below). For example `model_reasoning_effort = "low"` is the lowest effort the default model accepts.
- **Sandbox.** Codex on Linux warns when bubblewrap (`bwrap`, the tool Codex uses to confine its commands) is not installed; install your distribution's `bubblewrap` package. Without a working bubblewrap, Codex fails to sandbox and asks approval to read the plugin's `SKILL.md`: approve that first skill read. CrossTalk does not depend on bubblewrap. Both harnesses also show a folder-trust dialog the first time they run in a new directory.

## If something is wrong

| You see | Cause and fix |
|---|---|
| Server failed to start (`ENOENT`), or the model says a CrossTalk tool "isn't available" with no error | The registered binary is gone or moved. Run `crosstalk install --check`; K5 names the registered path. Run `crosstalk reinstall` from the binary you want, then restart the session. Claude Code caches a failed server for up to 15 minutes while `claude mcp list` says Connected: run `/mcp` and reconnect in the same session. |
| `agent_held` | A second CrossTalk server serves the session: a plugin from 0.1.2 that still starts its own server next to the installed one, or a project or local `crosstalk` entry. `crosstalk install --check` (K2) names it; update the plugin with `crosstalk install`, remove the extra entry, and restart. |
| A `codex exec` row with empty `cwd` and `status` and `reachable: false` | An exec session has no app server CrossTalk can reach: it can send, never receive, and its row can linger. Pick the interactive row instead. A `claude -p` or `codex --no-daemon` asker cannot receive the answer. |
| A Codex row with empty `cwd`, `reachable: false` after starting `codex -c ...` | Any `-c` override makes Codex run its own embedded app server instead of the managed daemon, on every platform, so CrossTalk cannot deliver to it. Put the setting in `~/.codex/config.toml` and start `codex` without `-c`. |
| `crosstalk: false` | No CrossTalk call yet, or a stale list: the list is a snapshot, and a peer's first call can take a minute to show, so try `ask` anyway or list again. It gets `send_message` but not `ask` until then. A prompt like "who are you?" can trigger Claude's native agent list and leave the session unbound; ask it to call CrossTalk's `list_agents` instead. |
| `native_inform_only` | The target has no CrossTalk binding. Use `send_message`; `ask` works after its first CrossTalk call. |

## Security in plain words

Every session on this machine runs as you, and any of them can message any other. A peer's text is data, never an instruction from you. Every delivered peer message carries a notice, and the server's instructions say the same; the plugin's skill repeats it when it loads (it fired in 0 of 6 replica runs, so the notice and instructions did the work). They tell the model not to read files, run commands or change anything because a peer asked, and to tell you and ask first. One exception: a peer may ask whether a session is online, its working directory, its git branch or its harness, and the model answers. File contents, listings, environment, command output and what you said stay with you until you allow them. Nothing in CrossTalk stops a message loop or bounds spend; a send result only nudges a model after 30 messages to one peer, then 60, 120 and so on, to check whether the exchange is progressing. End the session to stop a loop. The message notice does not stop a peer from asking this session to pass a message on to a third session; only the plugin's skill says not to, and it does not reliably load. Do not put secrets in messages.
