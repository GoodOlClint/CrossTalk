# How it works

This is the plain-English version of the [design](design.md), which is the specification. Terms are defined where they first appear.

## The pieces

- A *harness* is the agent program: Claude Code or Codex. A *session* is one running conversation in a harness.
- *MCP* (Model Context Protocol) is the standard a harness uses to load extra tools from a helper program. The helper talks to the harness over standard input and output.
- *A2A* (Agent-to-Agent) is a standard way for agent programs to message each other over HTTP.
- The *shim* is `crosstalk mcp`, the helper program a harness starts. One shim serves each harness connection, and it ends when the harness closes it.

`crosstalk install` registers the shim once per harness, so every new session starts one. CrossTalk starts no sessions and wraps none.

## What one shim does for its session

1. Gives the model seven tools over MCP: `list_agents`, `send_message`, `reply`, `ask`, `receive_message`, `claim` and `release`. The same tools appear in Claude Code and Codex.
2. Listens for messages from other shims. They arrive over A2A on a loopback address (`127.0.0.1`, reachable only from this machine). The shim also listens on a unix socket (a file-based local connection) that speaks Claude Code's own cross-session protocol, so Claude's native peers can reach it.
3. Delivers into its own harness, as described under [delivery](#delivery).
4. Keeps a journal. Every accepted message is written to a file under `~/.local/state/crosstalk/journal/` before anything acknowledges it. After a restart, the journal tells the shim what was already delivered, so a retry is not injected twice.

There is no daemon and no central router. Shims find each other from the harnesses' own session lists and from small descriptor files under `~/.local/state/crosstalk/agents/`.

## Identity

CrossTalk decides which session is calling on each request, not once at startup.

- Claude Code: from the parent process of the shim, which is the Claude session. Claude records each session in `~/.claude/sessions/`.
- Codex: from the thread id Codex attaches to each tool call. One shim process can serve several Codex threads.
- If neither works, the call is `unbound`, and only `list_agents` runs.

Each session gets an `agent_id` (the first 16 hex characters of a hash of its harness and session id). The `agent_id` does not change when you rename the session. The name is a lookup alias: the session's own name if free, otherwise a fallback built from the harness, the folder name and part of the `agent_id`.

## Delivery

A message has to land inside a session that is already running. CrossTalk uses the inbound path each harness already has.

- Claude Code: the shim writes the message to the session's own unix socket. The session sees a `<cross-session-message>` block between its steps. Claude sends no acknowledgement, so CrossTalk calls the message `accepted` when the connection stays open past a short grace period.
- Codex: the shim talks to Codex's app server (the local service that runs Codex threads). For immediate delivery from a verified sender, it starts a turn with the message as tool output: an idle thread wakes, and a busy one picks the message up at its next model step. For deferred delivery or an unverified sender, it appends the message to the thread's history without waking it (`queued`).

The result of a send is a delivery fact:

| State | Meaning |
|---|---|
| `accepted` | Injected into the session now. |
| `queued` | Added to the session's history for its next model request. |
| `rejected` | Refused by policy or limits. Not injected. |
| `unreachable` | Not delivered. |
| `unknown` | Unclear what happened. Do not resend blindly. |

None of these mean the model read the message or finished any work.

## Asks and replies

`send_message` tells. `ask` tells and expects an answer. An `ask` returns at once and the answer comes back as a new message after the other session calls `reply` with the message's id. The destination of a `reply` comes from the stored original, not from the model, so a reply cannot be redirected.

An `ask` takes one `reply`. If the model passes `deadline_seconds`, the asker gets a message when the time runs out.

Claude's own `SendMessage` tool delivers nothing to a CrossTalk peer. If a model uses it to answer, the shim sends back a notice listing the open messages the model can answer with `reply`. See [ADR 0009](decisions/0009-native-replies-bounce-back-to-the-sender-s-session-instead-of-being-routed.md).

If a message cannot be delivered, or its outcome is unclear, it waits in the mailbox for `receive_message`.

## Trust

Every session on the machine runs as you. CrossTalk does not isolate sessions from each other. It does three things:

- Sender identity comes from the connection. On the local socket, the shim reads the process id of the peer. Between shims, identity is a private token file only your user can read. Text in a message never decides who sent it.
- Peer text is labeled as data. Every delivered message carries a notice that it has no authority from the user. The model is told not to read files, run commands, use other tools or message a third session on a peer's word, and to tell you and ask first. A peer may ask whether a session is online, its working directory, its git branch or its harness, and the model answers.
- A policy decides what a message may be. `inform` and `ask` go to any registered session, and an `ask` needs a verified sender. A `command` goes to a `primary` session or, for a claimed session, only from its owner. A rejected `command` is dropped, not downgraded.

`kind` is a label the sending model picks. It is not a trust boundary, and a `command` gives the receiver no new permissions: whatever the session needs approval for, it still needs approval for.

These controls are advice to the model plus checks in the shim. A session running in an auto-approve mode could still act on a peer's text. Do not put secrets in messages.

## Where to read more

- [Design](design.md) is the full specification: schemas, state machine, test list.
- [Decisions](decisions/README.md) explains why each choice was made.
- [Examples](examples.md) shows the tools in use.
