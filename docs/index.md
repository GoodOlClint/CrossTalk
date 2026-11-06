# CrossTalk

CrossTalk lets Claude Code and Codex sessions on one machine message each other by name. A message lands inside the running session through the harness's own inbound channel (a harness is the agent program, such as Claude Code or Codex), never by screen scraping. It is one static binary, `crosstalk`, that each session starts as an MCP server (MCP is the Model Context Protocol, the way a harness loads tools).

## Install

Download the signed macOS binary or the Linux tarball from the [releases page](https://github.com/GoodOlClint/CrossTalk/releases), then run `./crosstalk install`. [Setup](setup.md) has the exact commands, the checks and troubleshooting.

## Read more

- [Setup](setup.md): install on macOS or Linux.
- [Design](design.md): components, message schema, trust model and tool surface.
- [Decisions](decisions/README.md): the architecture decision records.
- [Source on GitHub](https://github.com/GoodOlClint/CrossTalk).
