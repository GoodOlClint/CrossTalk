# Why CrossTalk

A coding agent works inside one session. It can read your files, run commands and edit code. It can message other sessions of its own harness, but not sessions of a different one: Claude Code cannot reach Codex, and Codex cannot reach Claude Code. If you want the two to cooperate, you do the carrying, or the agents pass state through files.

That is fine for one hand-off. It gets old when you run several sessions at once, or when you want an agent to get a second opinion without you stopping to relay it.

## The problem, in one example

You have two terminals open. In one, Claude Code is refactoring the billing module. In the other, Codex owns the database migrations. Claude needs to know whether a column is nullable.

Without CrossTalk, you switch to the Codex terminal, ask, copy the answer, switch back and paste it. The agent that needed the answer sat idle the whole time.

With CrossTalk, Claude asks Codex directly. The question lands in the Codex session. Codex answers. The answer lands in the Claude session. You can watch both, or not.

## What it changes

- Sessions can reach each other by name. `list_agents` shows every running session on the machine, its name, which harness it runs (a harness is the agent program, such as Claude Code or Codex) and its working directory. A message goes to a name.
- Claude Code and Codex message each other natively. Each already messages its own kind, but the two programs have no common messaging system. CrossTalk adds the path across, so state moves as messages instead of hand-off files.
- Messages arrive inside the running session, through that harness's own inbound path: the same way a message from its own kind arrives. The receiving agent sees them in its context between its own steps. Nothing types into the terminal and nothing starts a fresh `claude -p` or `codex exec` process per message.
- The sender is real. CrossTalk takes the sender's identity from the connection, not from what the message says. A message cannot claim to be from your other session by writing so.
- CrossTalk adds no daemon and no central router. Each session starts its own small CrossTalk process, and that process ends with the session.

## Use cases

### A second model checks the first

You ask Claude to finish a change. Before it calls the work done, it asks a Codex session to review it. The other model has different habits and different blind spots, so it catches different mistakes. Claude reads the review and fixes what is real.

The review request goes to the Codex session as an `ask`, and the review comes back as a `reply`.

### A coordinator hands work to workers

One session plans a job: five modules, five sessions. The coordinator tells each worker what to do with `send_message`, and each worker sends a report when it finishes. The coordinator can `ask` a worker how far along it is.

A coordinator can `claim` a worker. A claimed session is a helper whose owner is the coordinator, and only that owner may send it a `command` (see [how it works](how-it-works.md#trust) for what `command` does and does not mean).

### A question to a session that already knows

The Codex session in `~/Source/api` spent an hour learning how your auth middleware works. The Claude session in `~/Source/web` needs one fact from it. Asking the session that knows is cheaper than re-reading the code, and it is more accurate than a guess.

### A long job passed between sessions

You are about to run out of context in one session. You start a fresh one and have the old one send the new one a summary: what is done, what is next, what to avoid. The new session starts with the facts, not a blank page.

### A message from a script

A build script finishes and runs `crosstalk send my-session "build done, 3 tests failed"`. The session sees a message marked as unverified (CrossTalk could not tie it to a session) and decides what to do. See [the example](examples.md#from-a-shell-script).

## When not to use it

- Everything happens in one session. A single agent can use its own subagents. CrossTalk is for separate sessions.
- You need a hard limit on spend or on message count. CrossTalk has none. See the [FAQ](faq.md#is-there-a-limit-on-messages).
- The sessions are on different machines. CrossTalk is same-machine only.
- You need to isolate agents from each other. Every session runs as you, and any of them can message any other. CrossTalk gives you attribution, not isolation.

## Next

[See it work](examples.md), or [install it](setup.md).
