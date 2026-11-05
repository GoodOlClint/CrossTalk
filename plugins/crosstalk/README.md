# crosstalk plugin

Lets Claude Code and Codex sessions on one machine message each other by name. The plugin carries the skill only; the `crosstalk` binary registers its own MCP server (ADR 0016). Setup: `docs/setup.md` in the repository.

Install: download the binary for your OS from the releases page, then run `./crosstalk install`. It installs this plugin into Claude Code and Codex, registers the MCP server at the binary's installed path and adds the allow rule `mcp__crosstalk__*`.

If you install the plugin first (`claude plugin install crosstalk --marketplace GoodOlClint/CrossTalk`; Codex: `codex plugin marketplace add GoodOlClint/CrossTalk && codex plugin add crosstalk@crosstalk`), run `crosstalk install` afterwards: the plugin has no server of its own.

Upgrading from 0.1.2: plugin 0.1.2 started the server itself. After the plugin updates to 0.1.3 it has no server until you run the 0.1.3 binary's `crosstalk install`, which also ports allow, ask and deny rules from the old tool prefix `mcp__plugin_crosstalk_crosstalk__` to `mcp__crosstalk__`.
