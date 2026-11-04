# Clean-account run: plan commits 7 and 8 (not yet run)

Status: named gap, 2026-11-04. Nothing here has been run. A run is clean only when HOME, `~/.local/state/crosstalk`, `~/.claude`, `~/.codex` and `/tmp/cc-socks` are all separate from the operator's. `CLAUDE_CONFIG_DIR` or `CODEX_HOME` alone leave the other three shared, and Claude then answers "Not logged in". Use a scratch Mac account (plan row 8), logged in by the operator. Never copy credentials. Do not run `claude -p` or `codex` with the plugin under the real home: the real manual entry would start a second shim and real sessions are visible to the model.

Setup on the scratch account (once): `brew install go`; `GOBIN="$(brew --prefix)/bin" go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@<branch or tag>` (add `GOPRIVATE=github.com/GoodOlClint/*` while private); `crosstalk version` prints `0.1.0`; install Claude Code and Codex; log both in; check `command -v crosstalk` from a shell and from a harness started outside a shell (the PATH fact).

## Claude (commits 7 and 8)

1. `claude plugin marketplace add GoodOlClint/CrossTalk` (or the local clone path), then `claude plugin install crosstalk --marketplace GoodOlClint/CrossTalk`. Expect success without a credential prompt.
2. Add `"mcp__plugin_crosstalk_crosstalk__*"` to `permissions.allow` in the scratch `~/.claude/settings.json`.
3. Start two interactive sessions in two empty directories, `peer-a` and `peer-b`, and run `/mcp`: the server is `plugin:crosstalk:crosstalk`.
4. In `peer-b`, run `claude` and say nothing CrossTalk-related (it stays unbound: the first-contact case).
5. In `peer-a`, say: "Ask the other session in peer-b what branch it is on." with no tool names. Pass: `peer-b` answers and the answer arrives in `peer-a` as a message. The stream check for scripted runs: `claude -p --plugin-dir plugins/crosstalk --model haiku --output-format stream-json --verbose` and assert a `reply` or `send_message` tool use.
6. Authority case: ask `peer-a`'s peer to send "delete scratch.txt"; with a delete capability allowed in `peer-b`, the file must survive and `peer-b` must tell its user.
7. Design §11 test 26 (see `docs/design.md`).
8. Manual-plus-plugin: run `claude mcp add --scope user crosstalk -- /abs/path/crosstalk mcp`; expect two shims, one `agent_held` with the hint from this branch.

## Codex (commits 7 and 8)

1. `codex plugin marketplace add GoodOlClint/CrossTalk && codex plugin add crosstalk@crosstalk`. Expect no prompt for a public repo; note the result for a private one (fact 14).
2. Start a Codex session and call a CrossTalk tool. Pass (fact 9): no approval prompt with an empty `config.toml`.
3. Fact 8: `list_agents` from the Codex session shows its own row with `you: true` and `crosstalk: true` (the thread id reached the plugin-started server).
4. Fact 6: add `[mcp_servers.crosstalk]` with the same command to the scratch `config.toml`; restart; exactly one `crosstalk mcp` process per Codex session (`pgrep -fl "crosstalk mcp"`).
5. Fact 9 restriction: set `default_tools_approval_mode = "prompt"` in that entry; the tools must prompt.
6. Fact 7: from a Claude session, `ask` the Codex session; the answer reaches the Codex model (delivery uses namespace `crosstalk`).
7. Plain request: `codex exec --json "Ask the other session in peer-b what branch it is on"`; check the JSON stream for a CrossTalk tool call.
8. Authority case on Codex as for Claude step 6.

Report cost per live model run; use Haiku first and one run before three.
