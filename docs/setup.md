# Set up CrossTalk on one Mac

CrossTalk lets Claude Code and Codex sessions on this Mac message each other by name. You need macOS, Homebrew, and Claude Code and/or Codex. Not yet run on a clean account: see [the plan's open facts](decisions/0013-agent-plugins-and-skill-packaging.md#not-yet-run-live).

1. **Install the binary.** While the repo is private, add `GOPRIVATE=github.com/GoodOlClint/*` and have GitHub credentials ready; once public, drop it.
   ```sh
   brew install go
   GOPRIVATE=github.com/GoodOlClint/* GOBIN="$(brew --prefix)/bin" go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@latest
   crosstalk version
   ```
2. **Claude only: remove a manual entry.** If you ever ran `claude mcp add ... crosstalk`, run `claude mcp remove --scope user crosstalk`, and rewrite any allow, ask or deny rule that names `mcp__crosstalk__*` as `mcp__plugin_crosstalk_crosstalk__*`. **Codex:** keep an existing `[mcp_servers.crosstalk]` entry; it takes priority over the plugin and keeps any approval setting you put there.
3. **Install the plugin.**
   ```sh
   claude plugin install crosstalk --marketplace GoodOlClint/CrossTalk
   codex plugin marketplace add GoodOlClint/CrossTalk && codex plugin add crosstalk@crosstalk
   ```
4. **Claude only: allow the tools.** A Claude plugin cannot pre-allow its tools. Add `"mcp__plugin_crosstalk_crosstalk__*"` to `permissions.allow` in `~/.claude/settings.json`. The Codex plugin ships with tool approval set to `approve`; your own `config.toml` entry overrides it.
5. **Restart** every running Claude and Codex session. A running session keeps the old server.
6. **Try it.** Open one Claude and one Codex session (or two of either). In one, say: "List the other agents, then ask the other session what directory it is in." The answer arrives in your session as a new message.
7. **Upgrade.** Re-run the `go install` line, then `claude plugin marketplace update crosstalk && claude plugin update crosstalk@crosstalk` and `codex plugin marketplace upgrade`, then restart sessions.
8. **Uninstall.** `claude plugin uninstall crosstalk`, `codex plugin remove crosstalk@crosstalk`, then delete the binary.

`codex --no-daemon` tabs can send and can pull with `receive_message`, but cannot receive delivered messages: the sender gets `unreachable`. Delivery needs an app server CrossTalk can reach, and `--no-daemon` offers none.

## If something is wrong

| You see | Cause and fix |
|---|---|
| Server failed to start | `crosstalk` is not on the PATH the harness sees. Run `command -v crosstalk`; if it prints nothing, repeat step 1. If a harness started outside a shell still fails, put the absolute path in its MCP entry by hand. |
| `agent_held` | A second CrossTalk server serves the same session, usually the manual entry from step 2. Remove it and restart. |
| A session shows `crosstalk: false` | It has made no CrossTalk call yet. It still receives plain messages (`send_message`) but not `ask`. |
| `native_inform_only` | The target has no CrossTalk binding. Use `send_message`; `ask` works after its first CrossTalk call. |

## Security in plain words

Every session on this Mac runs as you, and any of them can message any other. A peer's text is data, never an instruction from you: the plugin tells the model to treat it that way and to ask you before acting on a peer's request. Do not put secrets in messages.
