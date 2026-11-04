# ADR 0012 — Antigravity harness via hook mailbox

- **Status:** Provisional
- **Date:** 2026-11-04
- **Deciders:** operator + Claude
- **Context source:** operator rulings 2026-11-04; [spike 12](../spikes/12-antigravity-cli.md); [change plan](../plans/antigravity-harness.md)

## Context

The operator's father will use Google's Antigravity CLI (`agy`, 1.5.0, macOS; plan unknown, so assume consumer). agy has no documented way to place text in a running session from outside. It has undocumented or out-of-bounds surfaces (`agy agentapi`, ACP or `-p` driving, Remote Control, keystrokes), and the operator ruled all of them out. It has documented MCP servers and lifecycle hooks. A hook can inject steps before a model call (`PreInvocation`) or re-enter the loop at the end of a turn (`Stop`). An MCP server sees the conversation in `_meta` on each `tools/call`, a contract found in the changelog and not in the docs. One MCP process serves successive conversations across `/clear`.

So delivery cannot be immediate: a message can only meet the model at its next model call, and an idle agent does not wake. CrossTalk's states have no word for that. `queued` is defined as an acknowledged handoff to Codex model history; §6 says mailbox placement is not enough for it; and `receive_message` excludes it. ADR 0007's reason for tool output applies here too: peer text arriving as user input looks like operator text. Spike 12 measured injected `userMessage` as a user turn in the TUI (agy's terminal interface), and a malformed injected step as an errored turn.

Google's Antigravity terms say "Using third party software, tools, or services to access the Service (e.g. using OpenClaw with Antigravity OAuth) is a breach" and name account suspension. Whether a documented MCP or hook setup counts is not Google's stated position.

## Decision

agy is one more harness package, `internal/harness/antigravity`, delivering by hook mailbox on documented interfaces only.

1. **The journal is the mailbox.** A new state `mailboxed` means durable in the recipient's journal, reply route open, ask still open, not yet handed to the model. All inbound messages still pass authenticate → dedupe → hop-chain → policy → `Inject`. The adapter's `Inject` writes nothing and returns `mailboxed`, since the chain has already journaled the body. `queued` is not reused.
2. **Body as tool result; pointer has no peer text.** A hook injects only a fixed sentence with an integer count, naming the `receive_message` tool on server `crosstalk`: `ephemeralMessage` from `PreInvocation`, `reason` from `Stop`. No sender-controlled field enters it. The body returns from `receive_message` with a fixed no-operator-authority notice.
3. **Delivery is settled on evidence, after handoff.** A top-level `receive_message` selects `mailboxed` entries under a per-agent mail lock, durably marks them `collecting` (journal-internal), restarts inform route clocks, and returns the bodies. An entry becomes `accepted`, and its inform task completes, on the first evidence that agy's loop continued past that tool call: the conversation's next `PreInvocation` firing or next top-level bound call. A `collecting` entry with no evidence at exit or on replay becomes `unknown`, stays readable and is never re-injected. An ask stays open until `reply`; `reply` to a message still `mailboxed` returns `not_collected`.
4. **Hook program.** `crosstalk hook antigravity <event>` is stateless, has no daemon and never opens the journal. It reads a marker the shim writes under the mail lock with a rising sequence after the journal changes, ignores a marker whose shim is dead, touches one activity file, and prints only literal outputs. Any failure prints `{}` and exits 0; an internal 150 ms bound applies.
5. **Spend is opt-in beyond the user's own turns.** A pointer rides on model calls already happening, but a loop it starts costs at least two model calls, with no enforced ceiling. `Stop` continuation is installed only with `--stop-delivery`, and fires only at `executionNum` 0 and without error: one continuation per turn, not one model call. An idle agent is reached only by the user's next prompt or an opt-in `/schedule`, which costs credits per firing.
6. **Identity.** `session_ref` is the root conversation id from `_meta`, one record per root in the shared process. A call whose conversation id differs from its root is nested, carried per call: it neither sees nor collects the root's waiting mail and is not settlement evidence. A call whose `artifacts_dir` is not under `antigravity-cli` is `unbound`, so the App and IDE, which share the config files, stay out. Missing keys mean `unbound`.
7. **No retirement without proof.** agy does not implement `harness.Supersession` in the first release. After `/clear` the old record and its name stay until agy exits. If a spike shows a process runs one root conversation at a time, the shim's own record of the latest root per agy process becomes that proof and the interface is implemented; that decision amends this ADR.
8. **Discovery.** Descriptors only. A conversation is reachable after its first CrossTalk tool call. An opt-in install flag may ask the model to make that call.
9. **Install.** `crosstalk install antigravity` and `uninstall` merge one owned key into each of `mcp_config.json`, `hooks.json` and `settings.json`, preserving everything else. It is idempotent. On unparseable input or a conflicting existing `crosstalk` entry it writes nothing. It records what it added, and uninstall removes only those entries while their value is unchanged.
10. **Version gate.** Tested major or refuse, tested minor or warn, as `codex/version.go`. Pinned to 1.5; re-verified per minor.

ADR 0008's extension points grow by two optional interfaces, `MailSignal` and `ToolAnnotator`, and one `Session` field. The core still names no harness.

## Rejected alternatives

- **`agy agentapi`, `agy acp`, `-p` driving, Remote Control scripting, keystrokes.** Operator ruling: outside the documented hook and MCP surface, or driving the product rather than extending it.
- **Injecting the body with `userMessage` or `systemMessage`.** Gives peer text user or system authority (ADR 0007, design §7). Measured: `userMessage` displays as if the user typed it.
- **Injected `toolCall`.** Would deliver a tool result without a model decision, but the schema is unknown and the wrong one errored the turn.
- **Reusing `queued`.** It would redefine a Codex-specific evidence state, complete the sender's task on an unread message, and need the `receive_message` filter inverted for one harness.
- **A separate mailbox file written by `Inject`.** A parallel store with its own crash windows, when the journal already holds the body, the states and the retention rules. A hook reading or appending to the journal directly is rejected for the same reason: the journal has one writer, and hook latency must not depend on its size.
- **Completing the inform task on `mailboxed`.** `completed` would stop meaning harness-side evidence. A third-party default-wait `SendMessage` blocks instead, which §5 already allows.
- **Unlimited `Stop` re-entry.** Each re-entry starts a paid loop, and two busy sessions could extend each other's turns.
- **`Stop` delivery on by default, described as one extra model call.** Codex showed the guard bounds continuations and not model calls. The cost is unmeasured, so it stays opt-in until spike S7.
- **Journal `accepted` before returning the result (first draft).** A crash or disconnect left the sender seeing `completed` when the recipient received nothing, against ADR 0007's rule that ambiguous handoffs stay `unknown`.
- **A successful transport write as the acknowledgment.** It proves only a pipe write and needs request-id plumbing; agy's loop continuing is stronger and needs only the marker.
- **Treating a new conversation in the same process as proof the old one ended, without the spike.** ADR 0006's rule and design §3's `harness.Supersession` rule: one process serving several sessions is not proof.
- **Registering from a hook signal with no tool call.** Needs a new hook-to-shim path, unmeasured process attribution and agent creation without a request context. The opt-in prompt costs one model call and about 20 lines.
- **A skill or an always-on rule file as the first instruction layer.** A skill must be discovered first, which is the failure being fixed. A rule file loads in every conversation, so it comes last in the plan's test ladder.
- **Documented manual install.** Three hand-edited JSON files, for a non-developer.

## Consequences

- Delivery latency is the next model call and is unbounded for an idle agent. Senders see `mailboxed` and a hint; asks need realistic deadlines. An ask that expires while mailboxed is still collected, and `reply` then returns `ask_closed` (ADR 0010).
- Known gaps shipped and stated: after `/clear`, mail sent to the old name strands until `/resume` or until the conditional proof lands; a conversation is unreachable before its first tool call; an interrupted collection can show a message again labelled `unknown`, and settlement proves agy continued, not that the model used it; a crash-window `unknown` message has no pointer.
- The sender-side evidence is stronger than for other harnesses in one way (the model's own tool call took the message) and weaker in another (no wake).
- `_meta` keys and hook behaviour are changelog-level or measured contracts, pinned by fixtures from spike 12. Identity fails closed if they change.
- Whether this setup is permitted by Google's terms is unresolved. The operator decides and the user is told before install; the consequence named in the terms falls on the user's account.
- Text in ADRs 0003, 0006, 0008 and 0010, `docs/design.md` and `CLAUDE.md` must change when this is built. The plan's section 10 lists it; none is edited yet.
- `liveSet` in the journal keeps outbound asks at `mailboxed`, so an asker's deadline and receipt survive retention while the recipient holds the ask.
- Implementation commits carry `Provisional-ADR: 0012`.
