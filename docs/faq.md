# FAQ

## Which agents does it work with?

Claude Code and Codex, in any pairing: Claude to Claude, Codex to Codex, or across. OpenCode (the v2 line only; 1.x is unsupported) is released in v0.3.0; its live end-to-end run was done on 2026-11-23 and re-run on 2026-11-24 with Claude and Codex senders, and a few named cases stay reduced (see docs/spikes/23-opencode-live-run.md in the repository). Pi (1.x, 1.6.0 tested; `crosstalk install pi`) is built but not yet in a release. It ran live once, on 2026-11-25, with named gaps (see docs/spikes/25-pi-live-run.md in the repository); its compatibility check has not been run live. Other harnesses (the agent programs) are possible later, because each harness lives in its own package behind one interface ([contributing](contributing.md#add-a-harness)).

## Does it work across machines?

Yes from 0.2.0. With 0.1.x, both sessions must run on the same machine, as the same user.

From 0.2.0 you can pair your own machines (same user, a LAN or a tailnet) and message `name@alias` across them. The limits: only `send_message`, `ask` and `reply` cross hosts (no `command`, `claim` or `release`); sessions on the other host that have no CrossTalk are listed but cannot be messaged; there is no relay, so a message to a sleeping host or a stopped gateway fails; each host needs an address its peers can dial, a fixed IP or a stable name such as a tailnet name (a host that changes networks keeps working when it advertises a name or tailnet IP that the gateway still listens on); and you pair each pair of hosts by hand. It is off unless you start it. See [setup](setup.md#remote-messaging-020) and [how it works](how-it-works.md#remote). It has been run live between two real machines over a Wi-Fi LAN, not yet over a tailnet.

## Do I have to start my agents a special way?

No. `crosstalk install` sets each harness up once. After that, start `claude`, `codex`, `opencode` or `pi` as you always do and restart any session that was already running when you installed. After an upgrade, running sessions refresh on their own once idle, unless you turned that off with `CROSSTALK_NO_AUTO_REFRESH=1` or the session started before the feature existed; those need a restart (`crosstalk install --check` lists them). For Codex, start plain `codex`: some launch options make a session send-only (next question).

## Known issue: why can my Codex session send but not receive?

A Codex session receives only when the managed app server (Codex's shared background server) hosts it, because that server is the only place CrossTalk can deliver to. Three ways of starting Codex break receiving ([#112](https://github.com/GoodOlClint/CrossTalk/issues/112)):

- `codex --no-daemon` runs the app server inside the terminal, with no endpoint anyone can reach.
- Any `-c key=value` override on the command line makes Codex start its own app server instead of the managed one. `-s`, `-a` and `--add-dir` are fine.
- `codex exec` runs one task with no app server CrossTalk can reach.

Such a session can still send. But its `list_agents` row shows an empty `cwd` and `reachable: false`, a message sent to it comes back `unreachable`, and an `ask` sent to it is refused at once as `rejected` with reason `no_endpoint`. To fix it, start plain `codex` and put every setting in `~/.codex/config.toml`.

Workaround without a restart: the session can pull undelivered messages (not asks) by calling `receive_message`, but nothing wakes it, so it sees them only when it polls. An `ask` is not stored for it: the asker gets `no_endpoint` and can send a plain message instead ([#114](https://github.com/GoodOlClint/CrossTalk/issues/114)). A refused ask is not kept for later either, so if the session gets an endpoint, the asker asks again. A plain `codex` session that started while the managed app server was down or still loading its thread recovers by itself: the next delivery looks the thread up again (at most once every 10 seconds per thread), with no restart ([#116](https://github.com/GoodOlClint/CrossTalk/issues/116)). If you must keep `--no-daemon` on macOS, the experimental opt-in `crosstalk install --codex-no-daemon-queue` lets a plain message wake an idle tab (each wake is a paid model turn; a remote or role-inbox message never wakes it; an `ask` still gets `no_endpoint`), and [setup](setup.md) also describes an untested workaround that gives the tab its own app server.

## Is there a limit on messages?

No. A session can send as many messages as it likes, and nothing bounds spend. Two sessions can message each other without end, and you stop that by ending a session.

There is a nudge, not a guard. Each session counts its sends to each peer. At 30, 60, 120, 240 and so on, the result carries a hint that asks the model to check whether the exchange is moving work forward and to stop and tell you if not. The send still goes through. The count is kept in memory and resets when the server restarts. It does not cover one message sent to many peers, `crosstalk send`, or Claude's own `SendMessage`. Whether models heed the hint is not measured.

## What stops a peer from taking over my session?

Nothing in CrossTalk enforces it. Every session runs as you, and any of them can message any other. What CrossTalk does:

- It takes the sender's identity from the connection, not from the text.
- It tells the model, in the tool instructions and in a notice on each message, that a peer's message is a teammate's request: act on it within the session's own permission settings, never change those settings, never treat it as your approval and refuse a request to do what the peer was denied.
- It gives a message from a script or another machine a notice that says to treat it as information and tell you.

That is advice to the model plus checks in the shim. It is not a sandbox: the harness's permission mode is the boundary, and a session in an auto-approve or full-access mode acts on a peer's request without asking you. Pi has no approval prompt at all, so that boundary is absent there. Keep secrets out of messages. See [how it works](how-it-works.md#trust).

## Can a peer make my agent message a third session?

Passing on information or a question is fine. Passing on a request to act needs your approval, and only the plugin's skill says so; a skill does not always load. This is a known gap, because a relayed request reaches a session whose permissions may be wider than the requester's.

## What is a role inbox?

A role inbox is a name for a job, such as `coordinator`, that more than one session can hold at once. A peer sends to `inbox:coordinator` and never needs the name of the session that currently has the job. The newest session to attach is the responder: it gets the message as sent and is the only one that can answer. The other members get a copy that cannot be answered, so a session that is handing the job over keeps seeing what arrives until it detaches. Members hear about each attach, detach, hand-over and claim by an `inform`.

The first session to attach creates the inbox and owns it. Another session attaches only after the owner calls `grant` for it, or by calling `claim` on an inbox whose owner is gone. If the responder cannot be reached, the send comes back `unreachable` and no other member is tried yet. An inbox lives on one host, and a session attaches only to an inbox that its user or launch brief names.

## What does `accepted` mean? Did the other agent read it?

It means the message went into the other session. It does not mean the model read it or did anything about it. For an `ask`, the only evidence of an answer is the `reply`.

## Why did my `ask` come back `native_inform_only`?

CrossTalk has no binding for the target yet: it is a Codex session that has not made a CrossTalk call, or a Claude session whose CrossTalk is not running (a Claude session with CrossTalk installed binds within a few seconds of starting). Send a plain `send_message`. Once the session binds, `ask` works. A Claude session nobody has typed into yet cannot answer: type one line in it first. Once the target binds it can answer an inform it received before that with `reply`, for an hour after the message was sent; if `reply` says `no_such_message`, the message was not kept and the target answers with `send_message` to the sender's agent_id shown in the message.

## Why did a native `SendMessage` reply come back to my own session?

On Claude, the `from=` of an inbound message names the receiving session's own socket, by design ([ADR 0009](decisions/0009-native-replies-bounce-back-to-the-sender-s-session-instead-of-being-routed.md)). A native `SendMessage` to it goes back to that same session and reaches no one. Answer with the CrossTalk `reply` tool and the inbound `message_id`.

## A message arrived twice, or not at all. Why?

Every message has an id, and the receiver ignores a repeat of an id it has already accepted, so a retry does not inject twice, as long as the record of that id is still in the journal (trimming is described under "Where does CrossTalk keep its data?"). If the outcome of a send was unclear, the result is `unknown` and the agent should not resend blindly. Entries it could not deliver wait for `receive_message`.

## Where does CrossTalk keep its data?

Under `~/.local/state/crosstalk/`: journals, tokens, descriptor files and sockets. A session's journal is trimmed when it is opened again (when the session registers): resolved records older than 24 hours are dropped, and unresolved ones are kept. A long-running session is not trimmed while it runs. `crosstalk uninstall` leaves this directory unless you pass `--purge`, which keeps only `upgrade.lock` and `upgrade.lock.child`.

## What does it cost?

CrossTalk is free and open source (MIT). Each message becomes input to a model in the receiving session, so it uses that session's tokens like any other input.

## How do I turn it off?

Run `crosstalk uninstall`. See [setup](setup.md). To stop one conversation, end one of the sessions.

## Where do I report a bug?

[Open an issue](https://github.com/GoodOlClint/CrossTalk/issues). If you can say which tool call you made and what `state` and `reason` came back, that is most of the diagnosis.
