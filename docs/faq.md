# FAQ

## Which agents does it work with?

Claude Code and Codex, in any pairing: Claude to Claude, Codex to Codex, or across. Other harnesses (the agent programs) are possible later, because each harness lives in its own package behind one interface ([contributing](contributing.md#add-a-harness)). None is planned for a release yet.

## Does it work across machines?

Yes from 0.2.0, which is not yet released. With 0.1.x, both sessions must run on the same machine, as the same user.

From 0.2.0 you can pair your own machines (same user, a LAN or a tailnet) and message `name@alias` across them. The limits: only `send_message`, `ask` and `reply` cross hosts (no `command`, `claim` or `release`); sessions on the other host that have no CrossTalk are listed but cannot be messaged; there is no relay, so a message to a sleeping host or a stopped gateway fails; each host needs a fixed IP address; and you pair each pair of hosts by hand. It is off unless you start it. See [setup](setup.md#remote-messaging-arrives-in-020-and-is-not-yet-released) and [how it works](how-it-works.md#remote). It has not been run live on two real machines yet.

## Do I have to start my agents a special way?

No. `crosstalk install` registers the CrossTalk server with each harness once. After that, start `claude` and `codex` as you always do and restart any session that was already running. Two limits apply to Codex: a tab started with `codex --no-daemon` can send but not receive, and any `-c` override on the command line makes Codex start its own app server, which CrossTalk cannot reach, so that session is send-only (`-s`, `-a` and `--add-dir` do not). Put settings in `~/.codex/config.toml` instead. Details are in [setup](setup.md).

## Is there a limit on messages?

No. A session can send as many messages as it likes, and nothing bounds spend. Two sessions can message each other without end, and you stop that by ending a session.

There is a nudge, not a guard. Each session counts its sends to each peer. At 30, 60, 120, 240 and so on, the result carries a hint that asks the model to check whether the exchange is moving work forward and to stop and tell you if not. The send still goes through. The count is kept in memory and resets when the server restarts. It does not cover one message sent to many peers, `crosstalk send`, or Claude's own `SendMessage`. Whether models heed the hint is not measured.

## What stops a peer from taking over my session?

Nothing in CrossTalk enforces it. Every session runs as you, and any of them can message any other. What CrossTalk does:

- It takes the sender's identity from the connection, not from the text.
- It labels every delivered message as data with no authority from you.
- It tells the model, in the tool instructions and in a notice on each message, not to read files, run commands or use any tool but CrossTalk's on a peer's word without asking you. The rule against messaging a third session is only in the plugin's skill.

That is advice to the model plus checks in the shim. It is not a sandbox. A session in an auto-approve mode could act on a peer's text. Keep secrets out of messages. See [how it works](how-it-works.md#trust).

## Can a peer make my agent message a third session?

The notice does not stop it. Only the plugin's skill tells the model not to, and a skill does not always load. This is a known gap.

## What does `accepted` mean? Did the other agent read it?

It means the message went into the other session. It does not mean the model read it or did anything about it. For an `ask`, the only evidence of an answer is the `reply`.

## Why did my `ask` come back `native_inform_only`?

The target is a session that has not made a CrossTalk call yet, so CrossTalk has no binding for it. Send a plain `send_message`. After the session's first CrossTalk call, `ask` works. A Claude session nobody has typed into yet cannot answer: type one line in it first.

## Why did a native `SendMessage` reply come back to my own session?

On Claude, the `from=` of an inbound message names the receiving session's own socket, by design ([ADR 0009](decisions/0009-native-replies-bounce-back-to-the-sender-s-session-instead-of-being-routed.md)). A native `SendMessage` to it goes back to that same session and reaches no one. Answer with the CrossTalk `reply` tool and the inbound `message_id`.

## A message arrived twice, or not at all. Why?

Every message has an id, and the receiver ignores a repeat of an id it has already accepted, so a retry does not inject twice, as long as the record of that id is still in the journal (trimming is described under "Where does CrossTalk keep its data?"). If the outcome of a send was unclear, the result is `unknown` and the agent should not resend blindly. Entries it could not deliver wait for `receive_message`.

## Where does CrossTalk keep its data?

Under `~/.local/state/crosstalk/`: journals, tokens, descriptor files and sockets. A session's journal is trimmed when it is opened again (when the session registers): resolved records older than 24 hours are dropped, and unresolved ones are kept. A long-running session is not trimmed while it runs. `crosstalk uninstall` leaves this directory unless you pass `--purge`.

## What does it cost?

CrossTalk is free and open source (MIT). Each message becomes input to a model in the receiving session, so it uses that session's tokens like any other input.

## How do I turn it off?

Run `crosstalk uninstall`. See [setup](setup.md). To stop one conversation, end one of the sessions.

## Where do I report a bug?

[Open an issue](https://github.com/GoodOlClint/CrossTalk/issues). If you can say which tool call you made and what `state` and `reason` came back, that is most of the diagnosis.
