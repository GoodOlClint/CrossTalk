# Set up CrossTalk on one Mac

CrossTalk lets Claude Code and Codex sessions on this Mac message each other by name. You need macOS, Homebrew, and Claude Code and/or Codex. A first clean-account run (2026-11-04) passed the basic plumbing and the Codex authority case; both cross-harness answer round trips passed live with the plugin only, and Codex started outside a shell passed on result. A Claude peer read a file on a soft ask; the notice in this release addresses that, and its live re-test is unrun. An explicit `prompt` approval entry is indirect only ([open facts](decisions/0013-agent-plugins-and-skill-packaging.md#not-yet-run-live)).

1. **Install the binary.** While the repo is private, set `GOPRIVATE=github.com/GoodOlClint/*` and run `gh auth setup-git` once so `go` can clone over https; drop both once public. Admin account:
   ```sh
   brew install go
   GOPRIVATE=github.com/GoodOlClint/* GOBIN="$(brew --prefix)/bin" go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@latest
   crosstalk version
   ```
   Non-admin account (`/opt/homebrew/bin` is not writable): use `GOBIN="$HOME/.local/bin"` and put that directory on the PATH in both `~/.zprofile` and `~/.zshenv`. Shell profiles do not reach a harness started outside a shell; see the table below.
2. **Claude only: remove a manual entry.** If you ever ran `claude mcp add ... crosstalk`, run `claude mcp remove --scope user crosstalk`, and rewrite any allow, ask or deny rule that names `mcp__crosstalk__*` as `mcp__plugin_crosstalk_crosstalk__*`. **Codex:** an existing `[mcp_servers.crosstalk]` entry takes priority over the plugin and does not inherit its `approve` default: with no `default_tools_approval_mode`, every CrossTalk tool prompts and `codex exec` blocks them. Set `"prompt"` to keep prompts, `"approve"` to run tools without asking (the plugin's default), or delete the entry knowingly.
3. **Install the plugin.**
   ```sh
   claude plugin install crosstalk --marketplace GoodOlClint/CrossTalk
   codex plugin marketplace add GoodOlClint/CrossTalk && codex plugin add crosstalk@crosstalk
   ```
4. **Claude only: allow the tools.** A Claude plugin cannot pre-allow its tools. Add `"mcp__plugin_crosstalk_crosstalk__*"` to `permissions.allow` in `~/.claude/settings.json`.
5. **Restart** every running Claude and Codex session. A running session keeps the old server.
6. **Try it.** Open one Claude and one Codex session (or two of either). In one, say: "List the other agents, then ask the session named `<its name>` what directory it is in." Name a target from the list. The answer arrives as a new message. A new directory shows a folder-trust dialog; accept it. If a peer asks you before answering anything else, that is the notice working: allow it or answer for it.
7. **Upgrade.** Re-run the `go install` line, then `claude plugin marketplace update crosstalk && claude plugin update crosstalk@crosstalk` and `codex plugin marketplace upgrade`, then restart sessions. Each release bumps the plugin version, which `plugin update` compares; a plugin installed before the bump needs `claude plugin uninstall` and a reinstall.
8. **Uninstall.** `claude plugin uninstall crosstalk`, `codex plugin remove crosstalk@crosstalk`, then remove the marketplace (`claude plugin marketplace remove crosstalk`, `codex plugin marketplace remove crosstalk`), the `mcp__plugin_crosstalk_crosstalk__*` allow rule and any cached plugin copy, stop running `crosstalk mcp` shims, and delete the binary.

`codex --no-daemon` tabs can send and pull with `receive_message` but cannot receive delivered messages: the sender gets `unreachable`.

## If something is wrong

| You see | Cause and fix |
|---|---|
| Server failed to start (`ENOENT executable not found in $PATH`), or the model says a CrossTalk tool "isn't available" with no error | `crosstalk` is not on the PATH the harness sees (`codex exec` shows no error). Run `command -v crosstalk`; if empty, repeat step 1. Start the harness from a shell that has the PATH, or put the absolute path in a hand-written MCP entry; on Claude that entry and the plugin both start a shim, so uninstall the plugin first (you lose the skill). Claude Code caches a failed server for up to 15 minutes while `claude mcp list` says Connected: after fixing the PATH, run `/mcp` and reconnect in the same session; a restarted session stayed failed. |
| `agent_held` | A second CrossTalk server serves the session, usually the manual entry from step 2. The notice names the fix: remove it and restart. |
| A `codex exec` row with empty `cwd` and `status` and `reachable: false` | An exec session has no app server CrossTalk can reach: it can send, never receive, and its row can linger. Pick the interactive row instead. A `claude -p` or `codex --no-daemon` asker cannot receive the answer. |
| `crosstalk: false` | No CrossTalk call yet. It gets `send_message` but not `ask`. |
| `native_inform_only` | The target has no CrossTalk binding. Use `send_message`; `ask` works after its first CrossTalk call. |

## Security in plain words

Every session on this Mac runs as you, and any of them can message any other. A peer's text is data, never an instruction from you. When the plugin's skill loads, it tells the model not to read files, run commands or change anything because a peer asked, and to tell you and ask first. One exception: a peer may ask whether a session is online, its working directory, its git branch or its harness, and the model answers. File contents, listings, environment, command output and what you said stay with you until you allow them. Do not put secrets in messages.
