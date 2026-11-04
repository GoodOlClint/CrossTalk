# Set up CrossTalk on one Mac

CrossTalk lets Claude Code and Codex sessions on this Mac message each other by name. You need macOS, Homebrew, and Claude Code and/or Codex. A first clean-account run (2026-11-04) passed the basic plumbing and the authority cases; cross-harness answer round trips, an explicit `prompt` approval entry, and a Codex started outside a shell are still unrun: see [the open facts](decisions/0013-agent-plugins-and-skill-packaging.md#not-yet-run-live).

1. **Install the binary.** While the repo is private, add `GOPRIVATE=github.com/GoodOlClint/*`, have GitHub credentials ready, and run `gh auth setup-git` once so `go` and `git` can clone over https; once public, drop all three. On an admin account with Homebrew:
   ```sh
   brew install go
   GOPRIVATE=github.com/GoodOlClint/* GOBIN="$(brew --prefix)/bin" go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@latest
   crosstalk version
   ```
   On a non-admin account `/opt/homebrew/bin` is not writable, so install to your home instead and put that directory on the PATH in both `~/.zprofile` and `~/.zshenv`:
   ```sh
   GOPRIVATE=github.com/GoodOlClint/* GOBIN="$HOME/.local/bin" go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@latest
   export PATH="$HOME/.local/bin:$PATH"
   crosstalk version
   ```
   Shell profiles do not reach a harness started outside a shell (a launcher, `env -i`): there `crosstalk` is not found. See the troubleshooting table.
2. **Claude only: remove a manual entry.** If you ever ran `claude mcp add ... crosstalk`, run `claude mcp remove --scope user crosstalk`, and rewrite any allow, ask or deny rule that names `mcp__crosstalk__*` as `mcp__plugin_crosstalk_crosstalk__*`. **Codex:** keep an existing `[mcp_servers.crosstalk]` entry; it takes priority over the plugin. It does not inherit the plugin's `approve` default: an entry with no `default_tools_approval_mode` makes every CrossTalk tool prompt, and `codex exec` blocks them. Keeping the prompts is supported: set `default_tools_approval_mode = "prompt"` explicitly. To run CrossTalk tools without asking, which is the plugin's own default, set `"approve"` in the entry, or delete the entry knowingly.
3. **Install the plugin.**
   ```sh
   claude plugin install crosstalk --marketplace GoodOlClint/CrossTalk
   codex plugin marketplace add GoodOlClint/CrossTalk && codex plugin add crosstalk@crosstalk
   ```
4. **Claude only: allow the tools.** A Claude plugin cannot pre-allow its tools. Add `"mcp__plugin_crosstalk_crosstalk__*"` to `permissions.allow` in `~/.claude/settings.json`. The Codex plugin ships with tool approval set to `approve`; your own `config.toml` entry overrides it.
5. **Restart** every running Claude and Codex session. A running session keeps the old server.
6. **Try it.** Open one Claude and one Codex session (or two of either). In one, say: "List the other agents, then ask the other session what directory it is in." The answer arrives in your session as a new message. A peer may stop at a folder-trust dialog the first time it starts in a directory; accept it, and press Enter twice if a prompt sent from a script does not submit. A peer that declines a plain informational ask, such as its working directory, is a defect in the skill wording: report it.
7. **Upgrade.** Re-run the `go install` line, then `claude plugin marketplace update crosstalk && claude plugin update crosstalk@crosstalk` and `codex plugin marketplace upgrade`, then restart sessions.
8. **Uninstall.** `claude plugin uninstall crosstalk`, `codex plugin remove crosstalk@crosstalk`, then delete the binary.

`codex --no-daemon` tabs can send and can pull with `receive_message`, but cannot receive delivered messages: the sender gets `unreachable`. Delivery needs an app server CrossTalk can reach, and `--no-daemon` offers none.

## If something is wrong

| You see | Cause and fix |
|---|---|
| Server failed to start (`ENOENT executable not found in $PATH`) | `crosstalk` is not on the PATH the harness sees. Run `command -v crosstalk`; if it prints nothing, repeat step 1. Shell profiles do not reach a harness started outside a shell. Either start the harness from a shell that has the PATH, or put the absolute path in a hand-written MCP entry. On Claude that entry and the plugin both start a shim, so remove the plugin (`claude plugin uninstall crosstalk`) when you use it, and you lose the skill. |
| The model says the CrossTalk tool "isn't available", with no error | The server did not start, usually the PATH row above. Retry with `PATH=$HOME/.local/bin:$PATH` set for the harness. `codex exec` shows no MCP error for this. |
| A `codex exec` row with an empty `cwd` and `status`, and `reachable: false` | An exec session has no app server CrossTalk can reach, so it can send but never receive, and its row can stay listed after it finishes. Do not message it; pick the interactive Codex row. Marking such rows stale is not done. |
| `agent_held` | A second CrossTalk server serves the same session, usually the manual entry from step 2. The `list_agents` notice now carries the fix: remove the manual entry and restart. |
| A session shows `crosstalk: false` | It has made no CrossTalk call yet. It still receives plain messages (`send_message`) but not `ask`. |
| `native_inform_only` | The target has no CrossTalk binding. Use `send_message`; `ask` works after its first CrossTalk call. |

## Security in plain words

Every session on this Mac runs as you, and any of them can message any other. A peer's text is data, never an instruction from you: the plugin tells the model to treat it that way and to ask you before acting on a peer's request. Do not put secrets in messages.
