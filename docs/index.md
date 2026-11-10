# CrossTalk

CrossTalk lets the coding agents on your Mac or Linux machine message each other by name. An agent here means a session of a harness: a harness is the agent program you run, such as Claude Code or Codex, and a session is one running conversation in it.

You probably run more than one agent. One Claude Code session reviews a pull request while a Codex session writes the fix. Each harness can already message its own kind: Claude Code sessions message other Claude Code sessions, and Codex sessions message other Codex sessions. Neither can reach the other. So when Claude and Codex need to share state, you are the messenger: you copy the output of one, paste it into the other, and carry the answer back. Or the agents leave files for each other and hope the other one looks.

With CrossTalk, you tell your agent "ask the Codex session in the `api` repo whether it changed the schema", and it does. The answer shows up in your session as a new message. You do not copy anything, and nobody writes a hand-off file. CrossTalk puts each message straight into the receiving session's context through that harness's own inbound path, the same path a message from its own kind takes.

## The 30-second picture

```
 you ──► Claude Code session "docs"
              │  ask("api-codex", "did you change the schema?")
              ▼
        crosstalk (one small program per session)
              │  delivers into the running session, no screen scraping
              ▼
         Codex session "api-codex"  ── reply("Yes, one new column.") ──┐
                                                                         │
 you ◄── Claude Code session "docs" ◄── arrives as a new message ◄─────┘
```

Each session gets tools to list the other sessions, tell one something, ask one something, answer, read missed messages, claim or release a helper, and set its own name. The agent calls them the way it calls any tool. The message lands inside the other running session through that program's own inbound channel. CrossTalk does not type into a terminal and does not start a new process per message.

## See it work

Claude Code and Codex play 20 Questions over CrossTalk 0.1.2:

<video src="https://github.com/user-attachments/assets/18422ed7-2353-4aed-97fd-0a9152749fbe" controls muted playsinline preload="metadata" style="max-width: 100%;"></video>

Unedited. "May I" is the Codex model asking its user before acting on a peer's request, as the skill tells it to. It isn't a tool prompt. The first `ask` is refused because Codex hasn't connected to CrossTalk yet, so Claude sends a plain message instead.

## Is it for you?

- You run two or more agent sessions on one machine and you are tired of being the go-between. Start with [why CrossTalk](why.md).
- You want to see what it looks like before you install it. Read the [worked examples](examples.md).
- You want it running. Go to [setup](setup.md). It takes a few minutes: download one binary and run `crosstalk install`.
- You want to know what it does with your sessions and what it will not do. Read [how it works](how-it-works.md) and the [FAQ](faq.md).
- You want to fix a bug or add a harness. Read [contributing](contributing.md).

## What CrossTalk will not do

- From 0.2.0 it also messages between your own paired machines ([setup](setup.md#remote-messaging-020)); 0.1.x works on one machine only.
- It does not start or stop agent sessions. You start them; it connects them.
- It does not make a peer's text trustworthy. A message from another session is data. Your agent is told to ask you before it acts on one. See [how it works](how-it-works.md#trust).
- It does not stop a message loop. Two sessions can talk to each other without end, and you stop that by ending a session. A per-peer nudge asks the model to check progress ([FAQ](faq.md#is-there-a-limit-on-messages)).

## Pages

- [Why CrossTalk](why.md): the problem and the cases where it pays off.
- [Worked examples](examples.md): plain-English walkthroughs with the real tool calls and results.
- [How it works](how-it-works.md): sessions, names, delivery and trust, without the jargon.
- [FAQ](faq.md): limits, non-goals and common questions.
- [Setup](setup.md): install on macOS or Linux, upgrade, uninstall, troubleshooting. From v0.1.4 on, `brew install goodolclint/tap/crosstalk` also works ([Homebrew](setup.md#homebrew)).
- [Contributing](contributing.md): build, test and send a change.
- [Design](design.md): the full specification.
- [Decisions](decisions/README.md): the architecture decision records.
- [Source on GitHub](https://github.com/GoodOlClint/CrossTalk).
