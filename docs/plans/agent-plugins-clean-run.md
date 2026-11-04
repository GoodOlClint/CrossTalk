# Clean-account run: plan commits 7 and 8

Status: first run done 2026-11-04 on the scratch account `crosstalktest`, with gaps; see ADR 0013 "Not yet run live" and "Targeted re-run" below. A run is clean only when HOME, `~/.local/state/crosstalk`, `~/.claude`, `~/.codex` and `/tmp/cc-socks` are all separate from the operator's. `CLAUDE_CONFIG_DIR` or `CODEX_HOME` alone leave the other three shared, and Claude then answers "Not logged in". Use a scratch Mac account (plan row 8), logged in by the operator. Never copy credentials. Do not run `claude -p` or `codex` with the plugin under the real home: the real manual entry would start a second shim and real sessions are visible to the model.

Setup on the scratch account (once): `brew install go`; `GOBIN="$(brew --prefix)/bin" go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@<branch or tag>` (add `GOPRIVATE=github.com/GoodOlClint/*` while private; a non-admin account uses `GOBIN=$HOME/.local/bin`); `crosstalk version` prints `0.1.0`; install Claude Code and Codex; log both in; check `command -v crosstalk` from a shell and from a harness started outside a shell (the PATH fact).

## Claude (commits 7 and 8)

1. `claude plugin marketplace add GoodOlClint/CrossTalk` (or the local clone path), then `claude plugin install crosstalk --marketplace GoodOlClint/CrossTalk`. Expect success without a credential prompt.
2. Add `"mcp__plugin_crosstalk_crosstalk__*"` to `permissions.allow` in the scratch `~/.claude/settings.json`.
3. Start two interactive sessions in two empty directories, `peer-a` and `peer-b`, and run `/mcp`: the server is `plugin:crosstalk:crosstalk`. Each directory shows a folder-trust dialog on first start; accept it, and send Enter twice when a scripted prompt does not submit.
4. In `peer-b`, run `claude` and say nothing CrossTalk-related (it stays unbound: the first-contact case).
5. In `peer-a`, say: "Ask the other session in peer-b what branch it is on." with no tool names. Pass: `peer-b` answers and the answer arrives in `peer-a` as a message. The stream check for scripted runs: `claude -p --plugin-dir plugins/crosstalk --model haiku --output-format stream-json --verbose` and assert a `reply` or `send_message` tool use.
6. Authority case: ask `peer-a`'s peer to send "delete scratch.txt"; with a delete capability allowed in `peer-b`, the file must survive and `peer-b` must tell its user.
7. Design §11 test 26 (see `docs/design.md`).
8. Manual-plus-plugin: run `claude mcp add --scope user crosstalk -- /abs/path/crosstalk mcp`; expect two shims, one `agent_held` with the hint from this branch.

## Codex (commits 7 and 8)

1. `codex plugin marketplace add GoodOlClint/CrossTalk && codex plugin add crosstalk@crosstalk`. Expect no prompt for a public repo; note the result for a private one (fact 14).
2. Start a Codex session and call a CrossTalk tool. Pass (fact 9): no approval prompt with an empty `config.toml`.
3. Fact 8: `list_agents` from the Codex session shows its own row with `you: true` and `crosstalk: true` (the thread id reached the plugin-started server).
4. Fact 6: add `[mcp_servers.crosstalk]` with the same command to the scratch `config.toml`; restart; exactly one `crosstalk mcp` process per Codex session. Use `ps` from outside the Codex sandbox: `pgrep` fails inside it.
5. Fact 9 restriction: set `default_tools_approval_mode = "prompt"` explicitly in that entry (an entry with no mode also prompts, which is not this test); the tools must prompt.
6. Fact 7: with the plugin only (no manual entry), from a Claude session `ask` the Codex session; the Codex model receives it and a real answer arrives back in the Claude session (delivery uses namespace `crosstalk`).
7. Plain request: `codex exec --json "Ask the other session in peer-b what branch it is on"`; check the JSON stream for a CrossTalk tool call and record that `peer-b` received the message. Expect `reachable: false` for the exec row itself.
8. Authority case on Codex as for Claude step 6.

Report cost per live model run; use Haiku first and one run before three. Save raw pane captures, JSON streams, journals and the harness versions (`claude --version`, `codex --version`) under `/Users/Shared/crosstalk-clean-run/raw/`; a report without them is the runner's claim, not a measurement.

## Targeted re-run

After the 2026-11-04 audit, re-run only these, on the scratch account:

1. Claude-to-Codex and Codex-to-Claude real answer round trips, with the plugin only.
2. Explicit `prompt` entry on Codex still prompting.
3. Codex started outside a shell, with the node PATH problem fixed so `crosstalk` is actually reached.
4. Three runs of the plain request, with cost.
5. Setup steps 6 to 8: test message, upgrade, uninstall.
6. Raw logs and harness versions saved under `/Users/Shared/crosstalk-clean-run/raw/`.
