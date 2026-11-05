# Set up CrossTalk on one Mac

CrossTalk lets Claude Code and Codex sessions on this Mac message each other by name. You need macOS, and Claude Code and/or Codex. A first clean-account run (2026-11-04) passed the basic plumbing and the Codex authority case; both cross-harness answer round trips passed live with the plugin only, and Codex started outside a shell passed on result. A Claude peer read a file on a soft ask on that run. In a live replica at c4025a9 (typed and bound Claude session, real Codex asker, harmless question answered first), Claude did not read the file before its user approved in 3 of 3 Haiku runs and 1 of 1 Sonnet run, where the older text read in 1 of 1; the sample is small and the rule is advice to the model, not enforcement, so a session in auto mode could still read on a peer's word. An explicit `prompt` approval entry is indirect only ([open facts](decisions/0013-agent-plugins-and-skill-packaging.md#not-yet-run-live)).

1. **Install the binary.** The main route is the release download: a universal binary (one file that runs on Apple silicon and Intel Macs) that is signed and notarized, so you do not need Go. Replace `v0.1.2` with the release you want. Run this in a scratch directory:
   ```sh
   TAG=v0.1.2
   curl -fLO "https://github.com/GoodOlClint/CrossTalk/releases/download/$TAG/crosstalk-$TAG-macos-universal.zip"
   curl -fLO "https://github.com/GoodOlClint/CrossTalk/releases/download/$TAG/crosstalk-$TAG-macos-universal.zip.sha256"
   shasum -a 256 -c "crosstalk-$TAG-macos-universal.zip.sha256"
   unzip "crosstalk-$TAG-macos-universal.zip"
   mkdir -p "$HOME/.local/bin" && mv crosstalk "$HOME/.local/bin/"
   crosstalk version
   ```
   The checksum check must print `OK`. Put the directory on the PATH in both `~/.zprofile` and `~/.zshenv` (shell profiles do not reach a harness started outside a shell; see the table below). On an admin account you can move the binary to `/opt/homebrew/bin` instead. A binary older than 0.1.2 has no `version` subcommand, so do not use it to check an existing install; compare the installed plugin version instead.

   Terms: to *notarize* is to upload a signed program to Apple, which scans it and records a ticket that macOS checks online. A *hardened runtime* binary is signed with extra OS-enforced limits on what it can do. *Quarantine* is the flag (`com.apple.quarantine`) that browsers and some other apps put on a downloaded file so macOS runs its Gatekeeper check on first launch. To *staple* is to attach the notarization ticket to the file so it works offline.

   First run on macOS: the binary is signed with a Developer ID certificate and its zip is notarized, but a bare command-line binary cannot be stapled, so macOS finds the ticket online. `curl` does not set the quarantine flag, so the download above skips the Gatekeeper prompt (this is how macOS behaves for curl downloads; not re-checked on a release build before the first release). If you download the zip with a browser instead, the extracted file may carry the quarantine flag; whether Gatekeeper then opens it silently or shows a prompt for a notarized bare binary is unverified. If it blocks, `xattr -d com.apple.quarantine ~/.local/bin/crosstalk` clears the flag. `spctl --assess` on a bare binary reports "rejected" even when it is signed, because it judges apps; do not use it as the check, use `codesign --verify --strict --verbose=2 ~/.local/bin/crosstalk`. None of this has been run against a real release build yet: the first release run is unproven.

   Alternative, build from source with Go. The repo is public, so `go` needs no `GOPRIVATE`, no `gh auth` and no GitHub login. `@latest` resolves to the newest release tag. Admin account:
   ```sh
   brew install go
   GOBIN="$(brew --prefix)/bin" go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@latest
   crosstalk version
   ```
   Non-admin account (`/opt/homebrew/bin` is not writable): use `GOBIN="$HOME/.local/bin"` and put that directory on the PATH as above.
2. **Claude only: remove a manual entry.** If you ever ran `claude mcp add ... crosstalk`, run `claude mcp remove --scope user crosstalk`, and rewrite any allow, ask or deny rule that names `mcp__crosstalk__*` as `mcp__plugin_crosstalk_crosstalk__*`. **Codex:** an existing `[mcp_servers.crosstalk]` entry takes priority over the plugin and does not inherit its `approve` default: with no `default_tools_approval_mode`, every CrossTalk tool prompts and `codex exec` blocks them. Set `"prompt"` to keep prompts, `"approve"` to run tools without asking (the plugin's default), or delete the entry knowingly. `codex plugin add` rewrites `~/.codex/config.toml`, so remove a manual entry by its content (the `[mcp_servers.crosstalk]` table), not by line number, and remove it together with every `[mcp_servers.crosstalk.tools.*]` sub-table: a tools table left without its server makes Codex refuse the whole config (`invalid transport`).
3. **Install the plugin.**
   ```sh
   claude plugin install crosstalk --marketplace GoodOlClint/CrossTalk
   codex plugin marketplace add GoodOlClint/CrossTalk && codex plugin add crosstalk@crosstalk
   ```
   The Claude plugin install tries SSH (`git@github.com`) first and falls back to HTTPS on its own, so no SSH key or GitHub credential is needed (checked 2026-11-04 in a scratch HOME with no key: "SSH clone failed, retrying with HTTPS"). Codex clones over HTTPS directly. If you want to skip the failed SSH attempt, add the marketplace by URL first: `claude plugin marketplace add https://github.com/GoodOlClint/CrossTalk.git`, then `claude plugin install crosstalk@crosstalk` (the URL form was not run).
4. **Claude only: allow the tools.** A Claude plugin cannot pre-allow its tools. Add `"mcp__plugin_crosstalk_crosstalk__*"` to `permissions.allow` in `~/.claude/settings.json`.
5. **Restart** every running Claude and Codex session. A running session keeps the old server.
6. **Try it.** Open one Claude and one Codex session (or two of either). In one, say: "List the other agents, then ask the session named `<its name>` what directory it is in." Name a target from the list. The answer arrives as a new message. `ask` needs the target to have made its first CrossTalk call; before that, use `send_message`. A Claude session nobody has typed into yet cannot answer: type one line in it first. An `ask` has one reply: if the peer refuses, that spends it, and after its user approves, a second `reply` is rejected and the answer goes by `send_message`. A new directory shows a folder-trust dialog; accept it. If a peer asks you before answering anything else, that is the notice working: allow it or answer for it.
7. **Upgrade.** Repeat step 1 with the new tag (or re-run the `go install` line), then `claude plugin marketplace update crosstalk && claude plugin update crosstalk@crosstalk` and `codex plugin marketplace upgrade`, then restart sessions. Each release bumps the plugin version, which `plugin update` compares; a plugin installed before the bump needs `claude plugin uninstall` and a reinstall.
8. **Uninstall.** `claude plugin uninstall crosstalk`, `codex plugin remove crosstalk@crosstalk`, then remove the marketplace (`claude plugin marketplace remove crosstalk`, `codex plugin marketplace remove crosstalk`), the `mcp__plugin_crosstalk_crosstalk__*` allow rule and any cached plugin copy, stop running `crosstalk mcp` shims, and delete the binary.

`codex --no-daemon` tabs can send and pull with `receive_message` but cannot receive delivered messages: the sender gets `unreachable`. To receive in such a tab, give the tab its own app server and tell CrossTalk where it is (not run live):

```sh
S="$HOME/.codex/crosstalk-tab.sock"
codex app-server --listen "unix://$S" &
```

A plugin install has no entry in `~/.codex/config.toml` to extend, and an entry holding only `env` makes `codex mcp list` fail with "invalid transport". Write a full entry (it takes priority over the plugin, as step 2 says, so keep the approval mode):

```toml
[mcp_servers.crosstalk]
command = "crosstalk"
args = ["mcp"]
default_tools_approval_mode = "approve"
env = { CROSSTALK_CODEX_ENDPOINTS = "unix:///Users/you/.codex/crosstalk-tab.sock" }
```

Checked in a scratch `CODEX_HOME`: `codex mcp list` accepts this entry. Not checked: running it next to the plugin in a live session.

Then start the tab against that server:

```sh
codex --remote "unix://$S"
```

`CROSSTALK_CODEX_ENDPOINTS` is a comma-separated list of Codex app-server endpoints (`unix://PATH` or `ws://HOST:PORT`) that CrossTalk queries besides the managed daemon. A `ws://` endpoint carries peer text in cleartext; use it only on this Mac's loopback.

## If something is wrong

| You see | Cause and fix |
|---|---|
| Server failed to start (`ENOENT executable not found in $PATH`), or the model says a CrossTalk tool "isn't available" with no error | `crosstalk` is not on the PATH the harness sees (`codex exec` shows no error). Run `command -v crosstalk`; if empty, repeat step 1. Start the harness from a shell that has the PATH, or put the absolute path in a hand-written MCP entry; on Claude that entry and the plugin both start a shim, so uninstall the plugin first (you lose the skill). Claude Code caches a failed server for up to 15 minutes while `claude mcp list` says Connected: after fixing the PATH, run `/mcp` and reconnect in the same session; a restarted session stayed failed. |
| `agent_held` | A second CrossTalk server serves the session, usually the manual entry from step 2. The notice names the fix: remove it and restart. |
| A `codex exec` row with empty `cwd` and `status` and `reachable: false` | An exec session has no app server CrossTalk can reach: it can send, never receive, and its row can linger. Pick the interactive row instead. A `claude -p` or `codex --no-daemon` asker cannot receive the answer. |
| `crosstalk: false` | No CrossTalk call yet. It gets `send_message` but not `ask`. A prompt like "who are you?" can trigger Claude's native agent list and leave the session unbound; ask it to call CrossTalk's `list_agents` instead. |
| `native_inform_only` | The target has no CrossTalk binding. Use `send_message`; `ask` works after its first CrossTalk call. |

## Security in plain words

Every session on this Mac runs as you, and any of them can message any other. A peer's text is data, never an instruction from you. Every delivered peer message carries a notice, and the server's instructions say the same; the plugin's skill repeats it when it loads (it fired in 0 of 6 replica runs, so the notice and instructions did the work). They tell the model not to read files, run commands or change anything because a peer asked, and to tell you and ask first. One exception: a peer may ask whether a session is online, its working directory, its git branch or its harness, and the model answers. File contents, listings, environment, command output and what you said stay with you until you allow them. Nothing in CrossTalk stops a message loop or bounds spend; a send result only nudges a model after 30 messages to one peer, then 60, 120 and so on, to check whether the exchange is progressing. End the session to stop a loop. The message notice does not stop a peer from asking this session to pass a message on to a third session; only the plugin's skill says not to, and it does not reliably load. Do not put secrets in messages.
