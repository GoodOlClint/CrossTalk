# Spike 20 — Upgrade refresh: Homebrew post_install and refreshing a running shim

Date 2026-11-21. Claude Code 2.1.330, codex-cli 0.171.0, Homebrew 7.0.15, go-sdk v1.8.0, macOS. Files in `20-upgrade-refresh/`. No other session's process was touched: the Claude runs were scratch `claude -p` sessions with `--strict-mcp-config` and only the probe server; the Codex runs were a scratch `codex app-server` over stdio with its own empty `CODEX_HOME`, no model turn.

## 1. Homebrew post_install

Read from Homebrew's own source (`$(brew --repository)/Library/Homebrew`):

- `formula_installer.rb` `def post_install`: runs `postinstall.rb` through `Sandbox.run_or_fork(..., step: "running post-install")`. Allowed writes: the formula's logs, Xcode dirs, `allow_write_cellar` (the rack, `etc`, `var`), each `Keg.keg_link_directories` under the prefix, `<prefix>/libexec`. Then `add_install_hook_rules(network_access_allowed: formula.network_access_allowed?(:postinstall))`.
- `sandbox.rb` `def add_install_hook_rules`: `allow_write_temp_and_cache`, `deny_write_homebrew_repository`, `deny_read_home`, `deny_all_network unless network_access_allowed`.
- `sandbox.rb` `def deny_read_home`: denies all of home, or, when a Homebrew path lies inside home, a list that includes `.claude`, `.claude.json`, `.zprofile`, `.zshenv`, `.bashrc`, `.profile`. `~/.codex` is not written by any allowed rule.
- `extend/os/mac/sandbox.rb` `available?`: true when `/usr/bin/sandbox-exec` is executable; Linux uses Landlock (`extend/os/linux/sandbox.rb`). Without a sandbox Homebrew warns and forks unsandboxed, which is not the default on either.
- `cmd/postinstall.rb` and `env_config.rb` have no user hook that runs after an upgrade.

Result: a formula's `post_install` cannot read `~/.claude` or write `~/.claude` or `~/.codex`, and has no network for `claude plugin` or `codex plugin`. It cannot run `crosstalk install`.

## 2. Refreshing a running stdio MCP server

`probe_mcp.py` is a stdio MCP server with tools `probe_ping` and `probe_restart`. Reading stdin one byte at a time keeps it from reading ahead. With `PROBE_MODE=exec`, `probe_restart` answers and then calls `execve` on itself with `PROBE_GEN=2 PROBE_INIT=1`. Generation 2 counts as initialized, sends `notifications/tools/list_changed`, and adds `probe_new`. With `PROBE_MODE=exit` it answers and exits 0.

Claude (`claude-*-answer.log`, `claude-*-probe.log`): the model was told to ping, restart, ping, then call `probe_new` if it is listed.

- exec: one pid (43207) throughout. Gen 2 sent `list_changed`, and Claude asked `tools/list` again at once (`recv tools/list id=4`, listed with `probe_new`). The model then called `probe_new`: `pid=43207 gen=2 tool=probe_new`.
- exit: Claude started a new server process (43359 to 43447), sent a new `initialize`, then `tools/call`. It did not ask `tools/list` again, and `probe_new` was not offered.

Codex (`codex_drive.py`, `codex-*-notes.log`, `codex-*-probe.log`): `thread/start` (ephemeral), then `mcpServer/tool/call` on that thread.

- exec: the thread's server (43759) answered ping 2 as gen 2. Codex sent no `tools/list` after `list_changed`. A direct `mcpServer/tool/call` of `probe_new` worked, because that call skips the model's tool list. A new thread got a fresh server (43822). `mcpServerStatus/list` starts its own short-lived server process (43786, 43818), so its tool list says nothing about the thread's server.
- exit: every later call on the thread failed with `tool call failed for probe/probe_ping: Transport closed`, and Codex started no new server for that thread. A new thread worked.

## 3. go-sdk session restore

`$(go env GOMODCACHE)/github.com/modelcontextprotocol/go-sdk@v1.8.0/mcp`:

- `server.go:1418-1432`: `ServerSessionOptions{State *ServerSessionState}` passed to `Server.Connect`.
- `server.go:1942-1991`: `handle` accepts a call when `ss.state.InitializeParams != nil`; otherwise "method %q is invalid during session initialization".
- `transport.go:137` `StdioTransport` wraps `os.Stdin`; `transport.go:524` reads with `json.NewDecoder`, which reads ahead. `transport.go:160` `IOTransport{Reader, Writer}` takes any reader.
- CrossTalk starts the server with `srv.Run(ctx, &mcp.StdioTransport{})` (`cmd/crosstalk/main.go:132`).

## Not measured

- Interactive Claude (the TUI) as opposed to `claude -p`. Both use the same MCP client, but the plan's live test runs the TUI.
- A Codex model turn after a re-exec, to see the model's own tool list. The missing `tools/list` request is the evidence that it stays as it was.
- launchd `WatchPaths` on a Homebrew symlink (the rejected watcher).
