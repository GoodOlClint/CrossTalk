# crosstalk plugin

Lets Claude Code and Codex sessions on one Mac message each other by name. The plugin starts `crosstalk mcp`, so the `crosstalk` binary must be on PATH (`go install github.com/GoodOlClint/crosstalk/cmd/crosstalk@latest`). Setup: `docs/setup.md` in the repository.

Install: `claude plugin install crosstalk --marketplace GoodOlClint/CrossTalk`; Codex: `codex plugin marketplace add GoodOlClint/CrossTalk && codex plugin add crosstalk@crosstalk`.
