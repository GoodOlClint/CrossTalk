# Change plan: CrossTalk 0.2.0 (remote: messaging between your own machines)

Status: draft for the brownfield gate, 2026-11-06. No implementation until the operator approves. It folds in the council decision (`~/.claude/coordinators/archive/council-decision.md`, chair Claude Opus 5.5 with two Codex advisors, 2026-11-06) and the operator rulings of 2026-11-06 (section 1.1), which win where they differ. The Codex critique disposition is section 13. Evidence tags: VERIFIED-SOURCE (read in this repo at f85f1cb), COUNCIL (verified by the council session, not re-read here), UNKNOWN (settled by the named spike).

Terms. **Host**: one of the user's own machines (laptop, desktop, homelab VM). **Shim**: the `crosstalk mcp` process a harness starts (ADR 0006). **Gateway**: the one network listener per host, run by `crosstalk remote serve`. **Peer host**: a host this host has paired with. **mTLS** (mutual TLS): an encrypted connection where both ends present a certificate. **Fingerprint**: the sha256 of a certificate's public key (SPKI, subject public key info), written as 64 lowercase hex. **Pinning**: accepting only a certificate whose fingerprint was recorded earlier, with no certificate authority. **Pairing**: the one-time exchange that records two hosts' fingerprints in each other's peers file. **Alias**: the short name a host is known by on its peers (default: the short hostname). **Tailnet**: a Tailscale private network; its addresses are in 100.64.0.0/10. **LAN**: the local network. **Principal**: the identity the receiver authorises. **Native session**: a harness session with no CrossTalk shim (`crosstalk: false`). **Login unit**: a launchd user agent (macOS) or systemd user unit (Linux) that starts a program at login. **Linger**: systemd's per-user setting that keeps user units running with nobody logged in. **Spike**: a small throwaway experiment that answers one unknown. **Slice**: one small pull request with its own tests. **DoD**: definition of done, what must be true, with evidence, for a slice to count. **Fails-before / passes-after**: the named test fails on the parent commit and passes on the slice's commit. **K1-K8**: the numbered checks `crosstalk install --check` prints. **S / M / L**: under 1 hour / about half a day / more than half a day.

## 1. Goal and scope

0.2.0 lets a Claude Code or Codex session on one of the user's machines find and message a CrossTalk session on another of the user's machines, by `name@host`, with `inform`, `ask` and `reply`, over the LAN or a tailnet. Nothing listens on the network unless the user turns remote on. No new harness ships in 0.2.0.

In:
- `crosstalk remote serve`: the gateway, one per host, started by the user (foreground) or by the opt-in login unit. TLS 1.3 pinned mTLS on one configured address and port (default 7745).
- Pairing: `crosstalk remote pair` on one host prints a one-time token; `crosstalk remote join` on the other reads it from stdin. `unpair`, `status`, `config`.
- Remote agent discovery: once two hosts are paired, `list_agents` on either shows the CrossTalk sessions on the other, fetched live (section 5). This is a required feature (operator clarification 2026-11-06), with its own slice (R6).
- Cross-host `inform`, `ask`, `reply` between CrossTalk-bound sessions, through the existing receiver chain, with dedupe, ask deadlines and journal-retried reply obligations.
- Addressing `name@host` (or `agent_id@host`); a bare name never leaves the host.
- The login unit: `crosstalk remote enable` / `disable`, and an optional remote step in `crosstalk install` (`--remote`, or an interactive question whose default is no).
- `crosstalk install --check` K8, and `crosstalk uninstall` removing what remote wrote.
- ADRs 0021 and 0022; ADRs 0001, 0003, 0004, 0006, 0011 amended; design.md, setup.md, how-it-works.md, faq.md, why.md, index.md updated by the slices.

Out of 0.2.0 (section 12 has the full list): messages to native sessions on another host; cross-host `command`, `claim`, `release`; any new harness; relay or store-and-forward while a host is down; Tailscale identity as authentication; cwd or paths in remote rows; **host** discovery (mDNS or `tailscale status` suggestions of machines to pair with, deferred to 0.2.x). Pairing stays the only way a host learns about another host; agent sessions on a paired host are discovered live and are in scope.

### 1.1 Operator rulings applied (2026-11-06)

| Ruling | Effect on the council design |
|---|---|
| Remote means the same user's own machines on a LAN or tailnet; not cloud sessions, not multi-user | One user, one trust domain across hosts; no per-user accounts, no CA |
| Any server component is opt-in, never installed or started by default | `crosstalk install` with no remote flag and no "yes" to the question writes nothing remote; nothing listens until `remote serve` or `remote enable` |
| The login unit is in 0.2.0, plus an optional remote step in `crosstalk install`, plus `remote enable` / `disable` | The council kept install unchanged and the unit droppable. Here the install option is designed (section 4.3) and the unit is not silently droppable: if spike S7 fails, the coordinator stops and asks the operator |
| Default gateway port 7745 | `remote.json` `port` defaults to 7745 |
| The homelab VM is Linux with a systemd user session; linger allowed, but CrossTalk prints `loginctl enable-linger` and never runs it | `remote enable` on Linux checks `loginctl show-user $USER -p Linger` and prints the command when it is `no` |
| Discovery means agent sessions on the paired machine (clarification) | Remote rows in `list_agents` are first-class (section 5, slice R6); only host discovery is deferred |
| Static IPs (2026-11-06): a host's address is its configured bind address, exchanged at pairing (A's in the token, B's in the pair request) | No `--address` option and no address-change handling: a moved host shows a clear `unreachable`, and pairing again (which updates the address for the same fingerprint) is the documented fix. The S2 address-change sub-test is dropped. mDNS broadcasts that refresh an already-paired fingerprint's address are deferred to 0.2.x and never add trust |

## 2. Existing state (map)

All VERIFIED-SOURCE at f85f1cb unless tagged.

| What | Where | Bearing on 0.2.0 |
|---|---|---|
| Per-agent A2A listener | `internal/a2a/server.go:206` binds `127.0.0.1:0`; `:208` URL is plain `http://` | Stays loopback-only. The gateway forwards to it; no shim ever listens off loopback |
| Request admission | `server.go:218` `admitted(limitBody(...))`; `:278` `admitted`, `:291` `limitBody` | The gateway applies size and connection limits before it parses, then the loopback server applies its own as today |
| Agent Card | `server.go:241` `loopbackCard`: 421 for a non-loopback `Host` (ADR 0011) | Unchanged; remote rows come from the gateway's authenticated list, never from the card |
| Authentication | `server.go:443-481` `authenticate`: peer token with no sender header → `local-user`; one `X-CrossTalk-Agent` header (`:34`) → read that agent's own token file and compare (`:465-478`) | A third case: the gateway credential, with gateway-set host and agent headers (slice R4) |
| Principal for the SDK | `server.go:422-427` `principal`: AgentID first, else Principal | Must include the host for a remote sender, or a remote agent could share task ownership with a local agent of the same agent_id |
| Sender and its key | `internal/receiver/receiver.go:49-56` `Sender`; `:61-68` `key()`: AgentID before Principal | Gains `Host` (fingerprint) and `HostAlias`; `key()` becomes `remote:<fingerprint>/<agent_id>` for a remote sender (COUNCIL risk 1) |
| Dedupe, receipt, route records | `receiver.go:235` dedupe keyed by `key`; `:267` receipt and `:291` route record `in.Sender.AgentID` | `:267` and `:291` must record the host-scoped key, or a reply route would point at a local agent with the same agent_id |
| Policy table | `receiver.go:370-399`: `inform` always allowed (`:372`), `ask` for verified senders, `command` owner or primary | Remote sender: `inform` and `ask` only; `command` refused with reason `remote_kind` |
| Peer and sender resolution | `internal/wire/wire.go:194` `resolvePeer`, `:206` `resolveSender`: local descriptors only; token path `:87-88`; peer token `:92` | Unchanged for local; remote senders never resolve through files |
| Send and settle | `wire.go:303-339` `SendTask`: refuses an empty `A2AURL` (`:304`), reads own token, posts, polls `GetTask` up to `DefaultSettle` 2 s (`:37`, `:319`) | A remote target goes to the peer's gateway with the host client key; settle bound measured by S5 |
| Transport call | `internal/shim/send.go:195-217` `viaTransport` | Gains a remote target path; `deliver` (`:222`) branches on a remote `Target` |
| Reply forwarding | `send.go:602-635` `forwardReply`: `descriptorByID(key)` (`:473-484`) lists local descriptors; not found → `unreachable/no_such_agent` | A remote key looks up the peer by fingerprint and sends to the agent_id through its gateway |
| Obligation retry | `send.go:637` `retryObligations`: runs once the agent's descriptor is published | A remote reply also needs a timed retry while the agent lives, since a sleeping host is common (slice R8) |
| Target resolution | `internal/mcpserver/target.go:37-66` `ResolveTarget`: registered name or agent_id, then native | `name@host` is parsed first and never falls through to local lookup; a bare name never matches a remote agent |
| `list_agents` | `internal/mcpserver/discovery.go:13-24` `Row`; `:28-50` tool output `{agents, warnings, notice}`; `:52-92` merge | Rows gain `host`; output gains `hosts`; remote rows come from the gateway, fetched in parallel with the local listing |
| Descriptor liveness | `internal/registry/registry.go:378` `Live`, `:411-427` `List` prunes, `:443-455` `prune` deletes | Remote agents are never written into the descriptor directory (it would prune them) |
| Agent identity | `internal/identity` agent_id = 16 hex of sha256(`harness:session_ref`) | Unique across hosts in practice, but not trusted as unique: keys add the host fingerprint |
| Harness list | `cmd/crosstalk/main.go:76-88` `harnesses`; subcommands `:45-56` | `remote` becomes a subcommand at `:45-56`; no harness change |
| Install flags | `cmd/crosstalk/install.go:27-29` usage, `:39-69` `parseInstallArgs` (harness names hard-coded at `:62`), `:89-247` `runInstall`, `:152` "neither claude nor codex" | The `--remote` and `--bind` flags edit `:27-29` and `parseInstallArgs`, the lines issue #42 also touches (section 4.3) |
| Uninstall | `install.go:299-342` `installRemoveSteps`: binary step, `--purge` state step | Gains removal of the login unit and, with `--purge`, of `~/.config/crosstalk` |
| Checks | `install.go:522-541` `installChecks`; K7 `installPathCheck` `:484-503` (warning only) | K8 added beside K7 |
| Docs and site tests | `cmd/crosstalk/site_test.go` `TestSiteLinks` (no link from published pages into `plans/`), `docs_test.go` `TestSetupDocCoversGaps` | Each docs slice keeps them green; new setup.md sections get a phrase test like `TestSetupDocCoversGaps` |
| a2a-go seams | v2.7.0 (`go.mod:6`): client takes a custom `http.Client` via `a2aclient.WithJSONRPCTransport`; server interceptors see headers only (`a2asrv.ServiceParams`) | COUNCIL. Confirms the gateway must terminate TLS in front of the SDK and pass identity on loopback by header (S1) |
| Go TLS resumption | `tls.Config.VerifyPeerCertificate` does not run on resumed connections; `VerifyConnection` does | COUNCIL (Go `crypto/tls` docs). Pin check goes in `VerifyConnection`, and session tickets are off |

Decision records read: 0001, 0003, 0004, 0006, 0008, 0009, 0010, 0011, 0014, 0016, 0018. Canonical pipelines (AGENTS.md): every inbound message goes authenticate → dedupe → hop chain → policy → inject; the gateway therefore never injects, it forwards to the target's loopback A2A server, which runs the chain. No harness-specific code is added outside harness packages; the login unit is per OS, not per harness, and lives in `internal/remote`.

## 3. Design

### 3.1 Pieces

| Piece | Runs as | Owns |
|---|---|---|
| Gateway | `crosstalk remote serve`, one per host, foreground or login unit | The TLS listener on `bind:port`; pin checks; pairing redemption; `GET /v1/agents`; forwarding `/v1/a2a/{agent}` to a local agent's loopback A2A server |
| Host identity | files in `~/.config/crosstalk/` | One ECDSA P-256 key and self-signed certificate per host, used as both the gateway's server certificate and every shim's client certificate |
| Remote client | inside each shim (`internal/remote` client) | Dials a peer's gateway directly with the host key, pins the peer's fingerprint |
| Gateway credential | `~/.local/state/crosstalk/gateway.token` (0600) | Proves to a loopback A2A server that a request came through this host's gateway |

`internal/remote` is the one new package (TLS config, pinning, peers file, pairing, gateway, client, login unit). No shim spawns or supervises the gateway (ADR 0006 holds); a shim works whether or not a gateway runs, it just cannot receive from other hosts without one.

### 3.2 Transport

- TLS 1.3 only (`MinVersion: tls.VersionTLS13`), server `ClientAuth: tls.RequireAnyClientCert`, `SessionTicketsDisabled: true`; the client sets no `ClientSessionCache`. Both sides set `InsecureSkipVerify: true` and do the pin check in `VerifyConnection`, which runs on every handshake including resumption.
- Server pin rule in `VerifyConnection`: the client's fingerprint is in `peers.json`, or a pairing is pending and unexpired (then the connection is marked unenrolled). Anything else fails the handshake, before any HTTP byte is read.
- Per request: the handler re-reads the pin against the current `peers.json` (cached by mtime and size), so `unpair` refuses requests on a connection that was opened before it. An unenrolled connection may call `POST /v1/pair` only.
- Bind: one configured IP address, which is also the address peers dial (static IPs, operator ruling). A wildcard (`0.0.0.0`, `::`) is refused, since peers could not dial it. Port default 7745.
- Limits before parsing: TLS handshake 5 s, header read 5 s, whole request including the body 10 s (`http.Server.ReadTimeout`), response 30 s, idle connection 60 s; body 80 KiB (64 KiB text limit plus envelope); 64 open connections for pinned peers, 8 concurrent requests and 120 requests per minute per peer host.
- Before classification: the listener admits at most 16 connections whose TLS handshake has not finished; a further connection is closed at accept, before any goroutine serves it. A slot is freed when the handshake finishes (the connection then moves to the pinned or the unenrolled pool, or is closed if neither has room) or fails, or at the 5 s handshake deadline. A pinned peer may therefore wait behind stalled handshakes for up to 5 s, which is accepted.
- Unenrolled connections (pairing pending) have their own pool, so they can never use up the pinned peers' connections: at most 4 at once, each closed 15 s after its handshake whatever it is doing, and none accepted once no pairing is pending (a pending pairing that expires closes the open ones). These numbers are guesses; S5 and the live run may move them.
- Gateway → loopback: `Authorization: Bearer <gateway.token>` plus headers the gateway sets itself after reading the sender's attested fields and deleting every inbound `X-CrossTalk-*` header (the sending host puts its agent fields under `X-CrossTalk-Remote-Agent-ID`, `-Name` (base64url), `-Harness` and `-Role` request headers, each exactly once, which the gateway reads, validates with the function the loopback server also uses and discards; a bad field is a JSON-RPC rejection `invalid_remote_sender` with HTTP 200, never a 400; the loopback names are distinct, so a forwarded header can never be mistaken for a gateway-set one; the gateway copies no inbound header to the loopback request): `X-CrossTalk-GW-Host` (fingerprint), `X-CrossTalk-GW-Alias` (from this host's own `peers.json`, never from the sender), `X-CrossTalk-GW-Agent-ID`, `-Name`, `-Harness`, `-Role` (attested by the sending host, validated: agent_id 16 lowercase hex; name any UTF-8 harness name as today (`identity.ChooseName` keeps it verbatim), carried base64url-encoded in the header and refused above 256 bytes or with control characters; harness `claude|codex`; role `primary|subagent`). Names never appear in a URL path.
- Forwarded methods: `SendMessage` and `GetTask` only. The gateway reads the JSON-RPC `method` and refuses anything else (`CancelTask`, `ListTasks`, streaming) with JSON-RPC `UnsupportedOperation`, and refuses a `SendMessage` carrying `metadata.crosstalk.claim`. The loopback receiver refuses `command` from a remote sender as well, so the rule holds even if the gateway check is wrong.
- Target: `{agent}` in the path is an agent_id only, looked up by the gateway in this host's own registry (`registry.List`, registered agents only). The gateway never dials a URL the caller supplies. A sender that has a name resolves it to an agent_id first through `GET /v1/agents` (section 8).

### 3.3 Identity, principal and keys

- The principal of a remote request is the host: `remote:<fingerprint>`, taken from the TLS handshake. The display alias comes from the receiver's own pairing record.
- A remote sender is `receiver.Sender{Verified: true, Host: <fingerprint>, HostAlias: <alias>, AgentID, Name: "<name>@<alias>", Role, Harness, Principal: "remote:<fingerprint>"}`. `key()` and the SDK principal are `remote:<fingerprint>/<agent_id>`, never agent_id-shaped, so a paired host can speak only as its own agents: it cannot pose as a local agent or as an agent on a third host.
- Within one host, which agent sent a message is attested by that host's binary, not proven: any process of that user that can read the host key could claim any agent_id of that host. This is the same-user strength design §5 already accepts for local senders.
- Dedupe, receipts, routes and reply obligations record the host-scoped key (`receiver.go:235,267,291`), so a reply route is keyed by (fingerprint, agent_id). It survives a gateway restart and an alias change (the alias is not in the key), and a new session that takes the same name gets a different agent_id and never receives the reply.

### 3.4 Addressing

- `to` accepts `name@host` or `agent_id@host`, where `host` is a peer alias in this host's `peers.json`. Local names may already contain `@` (`identity.ChooseName` and `registry.Rename` allow it), so resolution is: an exact local name match first, as today; else, if the part after the last `@` is a paired alias, the remote target; if both a local name and a remote target match the same value, `rejected/ambiguous_name` with a hint naming the local agent_id and `agent_id@host`. An alias may not contain `@`. A value with no paired alias after its last `@` is local only.
- A bare name never leaves the host. When it matches nothing locally, the error hint lists the `name@host` rows from the last remote listing that match it, so the model can retry with the full address.
- `from` on a remote envelope, as the receiving session sees it, is `name@alias`.
- An alias is unique in a host's `peers.json`; `join --alias` and `pair --alias` override the default.

### 3.5 Kinds

`inform`, `ask` and `reply` cross hosts. `command`, `claim` and `release` are refused with reason `remote_kind` (a hint says they are same-host only in 0.2.0). Removing `command` does not stop a compromised paired host from sending prompt-injection text as `inform` or `ask` (`receiver.go:372`, `:384`); the peer-text notice and each harness's own permissions stay the defence (COUNCIL risk 2).

## 4. CLI surface

Exit codes follow `crosstalk install`: 0 success, 1 failure, 2 usage error or no terminal to confirm on. Every command that writes asks for confirmation on a terminal unless `--yes`, and prints what it will write first, like `install`.

### 4.1 `crosstalk remote ...`

| Command | Does | Output | Exit |
|---|---|---|---|
| `remote config` | Prints the current config | `alias`, `bind`, `port`, fingerprint, config path | 0; 1 if `remote.json` is unreadable |
| `remote config --bind IP [--port N] [--alias NAME]` | Writes `remote.json`; creates the host key and certificate if missing. Changing `--bind` or `--port` later needs a fresh pair and join with each peer, which updates the address they hold | The written values, the fingerprint, and on a changed address `peers still dial the old address: pair again with <aliases>` | 0; 2 bad flag, `IP` a wildcard or not an IP on an interface of this host (allowed with `--force` for a host that is off its network now), or a port outside 1-65535 |
| `remote serve` | Runs the gateway in the foreground until SIGINT or SIGTERM. If the bind address is missing (Wi-Fi changed), it retries every 10 s and logs | One start line `gateway listening on ADDR:PORT, fingerprint FP`, then warnings | 0 on signal; 1 no config, or the port is held by another process for over 60 s |
| `remote pair [--alias NAME]` | Needs a running gateway on this host. Writes a pending pairing (secret, expiry; 0600) and prints a token; waits until it is redeemed or expires | `crosstalk-pair:v1:<address>:<port>:<fingerprint>:<id>:<secret>` on one line, then `paired with <alias> (<fingerprint>)` | 0 paired; 1 no gateway running, expired after 10 min, or interrupted (the pending entry is removed) |
| `remote join [--alias NAME]` | Prompts `Paste the token from the other host:` and reads one line from stdin (never an argument, so it stays out of shell history); dials the other host's gateway, checks its fingerprint against the token, proves the secret; writes the peer | `paired with <alias> (<fingerprint>) at <address>` | 0 paired; 1 bad token, wrong fingerprint, refused, unreachable; 2 no remote config here (run `remote config --bind` first) |
| `remote unpair ALIAS` | Removes ALIAS from this host's `peers.json`. Open connections from it are refused at the next request | `unpaired ALIAS; run crosstalk remote unpair <this alias> on ALIAS too` | 0; 1 unknown alias |
| `remote status [--json]` | Config, fingerprint, gateway state (`not configured`, `not running`, `running`, `service running, port not bound`), login unit state, and each peer probed with a 2 s timeout | One block per section; `--json` the same as an object | 0 nothing wrong; 1 configured but something is wrong (gateway down while the unit is enabled, a peer unreachable is only a warning) |
| `remote enable [--bind ADDR ...] [--yes]` | Writes config if flags are given (as `config`), then the login unit pointing at the installed binary, then starts it. Refuses to overwrite a unit it did not write (section 4.2) | The unit path, its content, the start result; on Linux with `Linger=no`, the line `run: loginctl enable-linger <user>` | 0; 1 refused or the unit failed to start; 2 no config and no `--bind` |
| `remote disable [--yes]` | Stops and removes the unit CrossTalk wrote. Keeps config, key and peers, so `enable` brings it back | `removed <unit path>` or `no CrossTalk login unit` | 0; 1 the unit on disk is not the one CrossTalk wrote (left in place) |

Unknown subcommand or no subcommand: usage on stderr, exit 2.

Peer edits (operator note 2026-11-06): v0.2.0 has no command to edit a paired peer's alias or address. The fix is to pair and join again, or to hand-edit `peers.json` (setup.md documents the fields; the gateway and shims re-read it when its mtime or size changes, and a file that does not parse is refused with its path, never half-read). A general `crosstalk config` that views and sets settings and edits peers is issue #44, after v0.2.0.

Forward compatibility with #44: `remote.json` and `peers.json` carry `"version": 1` and flat keys named as #44 would address them (`remote.bind`, `remote.port`, `remote.alias`; `peers.<alias>.address`, `peers.<alias>.alias`), so a later `crosstalk config` reads and writes the same files with no migration. `crosstalk remote config` stays as a supported form when #44 lands (it can become a thin alias for `crosstalk config remote.*`); no v0.2.0 command name or flag is planned to be removed, and the fingerprint, never the alias, stays the key every record uses, so renaming a peer later breaks nothing.

### 4.2 Login unit

- macOS: `~/Library/LaunchAgents/dev.getcrosstalk.remote.plist`, `RunAtLoad` true, `KeepAlive` `{SuccessfulExit: false}`, stdout and stderr to `~/Library/Logs/crosstalk-remote.log`; loaded with `launchctl bootstrap gui/<uid>`, removed with `launchctl bootout`.
- Linux: `~/.config/systemd/user/crosstalk-remote.service`, `ExecStart=<bin> remote serve`, `Restart=on-failure`, `WantedBy=default.target`; `systemctl --user daemon-reload` then `enable --now`. Linger is reported, never set.
- `<bin>` is the same absolute path `install` registers (`~/.local/bin/crosstalk`, or `<brew prefix>/bin/crosstalk` for a Homebrew binary, ADR 0018), checked executable and answering `crosstalk version` with this build's version before the unit is written.
- Ownership: the unit's sha256 is stored in `remote.json` (`unit_sha256`). `enable` with an identical file is a no-op; a file with another hash (user-edited or foreign) is refused and nothing is written; `disable` and `uninstall` remove only a file whose hash matches.

### 4.3 The install option (operator ruling)

- Flags: `crosstalk install --remote --bind ADDR [--port N] [--alias NAME]` and `crosstalk reinstall --remote ...`. `--bind` is required with `--remote --yes`.
- Interactive (a terminal, no `--yes`, no `--remote`): after the PATH step, one question: `Set up remote messaging to your other machines? This starts a network listener at login. [y/N]`. Default no. On yes it lists candidate addresses found on this host's interfaces (tailnet 100.64.0.0/10 first, then private LAN addresses; never 0.0.0.0) and asks which to bind.
- With `--yes` and no `--remote`, install never asks and never writes anything remote. Nothing remote is the default in every mode.
- What it writes: a `remote` step group running the same steps as `remote enable` (config, host key and certificate, login unit, start), each shown before it is applied. It does not pair; it ends with `next: crosstalk remote pair on this host, crosstalk remote join on the other`.
- `install --check` K8 (section 4.4).
- `uninstall`, one rule: the login unit goes when CrossTalk has no remaining user, which is the binary step's `installKeep` decision (`install.go:299-322`), not whether the file is deleted. When `installKeep` keeps the binary (another harness or a leftover project registration still runs it), the unit is kept; otherwise the unit is stopped and removed first, whether the uninstall named a harness or not, and also when Homebrew owns the binary and keeps the file (`install.go:311-313`). A unit whose hash does not match `unit_sha256` is never removed: uninstall prints its path and that it points at a removed binary.
- `uninstall --purge`: `~/.config/crosstalk/` follows the existing state step's rule (`install.go:323-341`: kept while another harness is set up or a shim holds an agent), plus one more guard: it refuses while a gateway runs. `remote serve` holds `~/.local/state/crosstalk/gateway.lock` (an exclusive `flock`) for its lifetime, also the one-gateway-per-host guard, and purge checks it as it checks `HeldAgents`, so a foreground gateway or one under an edited unit stops the purge with `a gateway is running (pid N); stop it, then run uninstall --purge again`. Config is deleted only together with the state directory; it then prints `unpair this host on: <aliases>`, since peers still pin its old key.
- Issue #42 (install hard-codes harness names at `install.go:27-29,62,152`): this option edits the usage string at `:27-29` and adds flags to `parseInstallArgs` next to `:62`. Slice R11 adds no harness name and does not fix #42; if #42 lands first, R11 rebases onto it.

### 4.4 K8

`K8 crosstalk <PASS|WARN|FAIL> <detail>`, one line per finding, printed after K7:

| State | Line |
|---|---|
| No `remote.json` | `K8 crosstalk PASS remote not configured` |
| Config unreadable, key not 0600, certificate does not match key, `peers.json` unreadable or not 0600 | FAIL with the path |
| Bind address not on any interface | WARN (laptop off its network) |
| Unit enabled but its hash does not match `unit_sha256`, or its binary path is not the installed binary | FAIL |
| Gateway not answering on `bind:port` | WARN when no unit is enabled (foreground use is valid); FAIL when the unit is enabled |
| Linux, unit enabled, `Linger=no` | WARN with the `loginctl enable-linger` command |

## 5. Remote agent discovery (`list_agents`)

Required in 0.2.0 (operator clarification). Hosts are known only by pairing; the agent sessions on each paired host are listed live.

- Source: each paired host's gateway answers `GET /v1/agents` (pinned mTLS like every route) with every session `list_agents` shows locally, registered and native, cut to `agent_id`, `name`, `harness`, `crosstalk`, `role`, `reachable`, `status`. No cwd, no paths, no pid, no session_ref, no endpoint (ADR 0011 amendment).
- Native sessions on the remote host are listed (operator ruling 2026-11-06) as rows that cannot be messaged from here: `crosstalk: false`, `messageable: false`, and `note: "no CrossTalk in this session; it cannot be messaged from another host"`. Delivery stays CrossTalk-only: a send or ask to such a row is `rejected/remote_native_unsupported` with a hint, and nothing is injected. A row that can be messaged has `messageable: true`.
- Row shape in `list_agents` output: `{"name": "<name>@<alias>", "host": "<alias>", "harness": "claude|codex", "agent_id": "<16 hex>", "crosstalk": <bool>, "messageable": <bool>, "reachable": <bool>, "status": "...", "role": "..."}`, plus `note` on a native row; `role` only on a CrossTalk row. Local rows gain no `host` field (absent = this host). `name@alias` is exactly what `send_message(to)` accepts.
- Output gains `hosts`: one entry per paired host, `{"host": "<alias>", "reachable": <bool>, "agents": <count>, "error": "<reason>"}`; reasons `gateway_not_running` (connection refused), `timeout`, `tls_pin_mismatch`, `unpaired_by_peer` (the peer refused our certificate), `unreachable`.
- Freshness: fetched on every `list_agents` call, no cache. All peers are asked in parallel and in parallel with the local listing; the call waits at most 1.5 s for remote hosts, then returns whatever arrived. A host that did not answer in time shows `reachable: false, error: "timeout"` and contributes no rows. Local rows never wait on a remote host beyond that bound.
- Gateway not running on the remote host: refused fast, `error: gateway_not_running`, no rows. This host's own gateway not running does not stop listing others (listing is outbound only), but `hosts` entries carry a warning that this host cannot receive.
- `reachable` on a remote row means the remote host answered and its own listing marks the agent reachable; it is not a delivery promise.
- Known shortcut: no cache and no backoff for a sleeping host, so each `list_agents` with a sleeping peer pays up to 1.5 s; add a 30 s negative cache per host if the live run shows it hurts.

## 6. Configuration and file layout

| Path | Mode | Written by | Content |
|---|---|---|---|
| `~/.config/crosstalk/` | 0700 | `remote config`, `enable`, `install --remote` | Directory |
| `~/.config/crosstalk/remote.json` | 0600 | `remote config`, `enable` | `{"version": 1, "alias", "bind", "port": 7745, "unit_sha256"}` |
| `~/.config/crosstalk/host.key` | 0600 | first `remote config` | ECDSA P-256 private key, PEM |
| `~/.config/crosstalk/host.crt` | 0644 | first `remote config` | Self-signed certificate for that key, 20-year validity (only the fingerprint is checked) |
| `~/.config/crosstalk/peers.json` | 0600 | `pair`, `join`, `unpair` | `{"version": 1, "peers": [{"alias", "address", "fingerprint", "paired_at"}]}`, written atomically (temp + rename) under `peers.lock` |
| `~/.local/state/crosstalk/gateway.lock` | 0600 | `remote serve` (held with `flock` while it runs) | Empty; one gateway per host, and `uninstall --purge` refuses while it is held |
| `~/.local/state/crosstalk/gateway.token` | 0600 | `remote serve` (exclusive create, kept across restarts) | Gateway credential for loopback A2A servers |
| `~/.local/state/crosstalk/remote/pending.json` | 0600 | `remote pair`, redeemed by the gateway | `[{"id", "secret", "expires_at"}]`, edited under `pending.lock`; an entry is deleted on redemption, expiry or interruption of `remote pair`, never by a failed attempt |
| `~/Library/LaunchAgents/dev.getcrosstalk.remote.plist` or `~/.config/systemd/user/crosstalk-remote.service` | 0644 | `enable` | Login unit (section 4.2) |

Every file is refused if it is not a regular file with the expected mode, like the uds key (design §5). Every path is under the user's home; nothing needs root.

## 7. Pairing protocol

1. On host A, `remote pair` makes a 32-byte random secret, stores the secret itself (the gateway needs it as the HMAC key; a hash could not verify the proof) and an expiry 10 minutes ahead in `pending.json` (0600, the same protection as the host key), and prints the token `crosstalk-pair:v1:<A bind>:<A port>:<A fingerprint>:<id>:<secret base64url>`.
2. The user copies it to host B and runs `remote join`, which reads it from stdin.
3. B dials A's gateway. B's `VerifyConnection` requires A's server fingerprint to equal the token's. A's `VerifyConnection` admits B's unpinned certificate only because a pairing is pending; the connection is unenrolled.
4. B sends `POST /v1/pair {"id": <id from the token>, "alias": <B alias>, "address": <B bind:port>, "mac": HMAC-SHA256(secret, "crosstalk-pair-v1" || B fingerprint || A fingerprint)}`. A takes B's fingerprint from the handshake, never from the body, so the secret is bound to B's key.
5. A, under `pending.lock`, finds an unexpired entry whose secret verifies the MAC (constant-time), removes it, then writes B into `peers.json`. Single use: two concurrent redemptions of one secret cannot both find the entry. A wrong MAC or an unknown id never removes or changes an entry, so a client without the token cannot cancel a pairing; the request names the entry by its `id` (in the token). Guessing is pointless against a 256-bit secret, and `POST /v1/pair` is rate-limited to one attempt per second for the whole gateway, which also bounds log noise.
6. A answers `{"alias": <A alias>}`; B writes A into its `peers.json`; A's waiting `remote pair` prints `paired with <B alias>` and exits 0.
7. Re-pairing a fingerprint already in `peers.json` updates its alias and address rather than adding a second entry. If B's write fails after A's, A has B and B does not; running pair and join again repairs it.

Both hosts need `remote config --bind`: B sends its own bind address and port so A can reach B for replies. `join` refuses (exit 2) without a config.

## 8. Message flow across hosts

Host A runs `alice` (Claude), host B runs `bob` (Codex); A and B are paired; both run a gateway.

Inform, `send_message(to="bob@B", text)` from alice:
1. A's shim parses `bob@B`, finds B in `peers.json`, and resolves `bob` to an agent_id with `GET /v1/agents` on B's gateway (exactly one registered agent of that name; a match that is only a native session is `rejected/remote_native_unsupported`; none is `no_such_agent`; several is `ambiguous_name`). It journals the outbound record with `to` = `remote:<B fp>/<bob agent_id>` before sending, so receipts (`receiver.askedPeer`, `receiver.go:332-335`) and polling use the identifier, never the name.
2. It posts `SendMessage` (`returnImmediately: true`) to `https://<B address>/v1/a2a/<bob agent_id>` with A's host certificate, pinning B's fingerprint, with headers attesting alice's agent_id, name, harness, role.
3. B's gateway checks the pin, strips and rebuilds the `X-CrossTalk-*` headers, looks up bob's agent_id (from the path) in B's registry, and forwards to bob's loopback `a2a_url` with the gateway credential.
4. Bob's A2A server authenticates the gateway credential, builds the remote `Sender`, and runs the receiver chain: dedupe on (`remote:<A fp>/<alice id>`, message_id), hop chain, policy (`inform` allowed), inject into Codex. It answers `submitted`; the gateway returns that to A.
5. A's shim polls `GetTask` at the same agent_id path (a name reused on B meanwhile cannot redirect the poll) up to the settle bound and returns `accepted`, `queued`, `unknown/pending`, `rejected` or `unreachable`. A gateway acceptance alone is never reported as delivery.

Ask, `ask(to="bob@B", text, deadline_seconds=600)`: as inform with `kind: ask`; bob's receiver opens the route keyed by `remote:<A fp>/<alice id>`. Alice's deadline runs on A's clock only (design §8); no timestamp from B is compared with A's clock.

Reply, bob calls `reply(message_id, text)`:
1. Bob's shim journals the obligation, completes the ask's A2A task on bob's server (alice's `GetTask` would see `completed`), and finds the route's key is remote.
2. It looks up A by fingerprint in B's `peers.json` (the bind address it sent at pairing) and posts the reply as an `inform` with `in_reply_to` to `https://<A address>/v1/a2a/<alice agent_id>`, by agent_id, never by name.
3. A's gateway forwards to alice's loopback server; alice's receiver injects it. If alice's session is gone, A's gateway answers `rejected/no_such_agent` and B's obligation records it; a new session that took the name `alice` has another agent_id and never gets it.
4. Not delivered (A asleep, gateway down): the obligation stays pending and is retried (slice R8): at bob's next registration, and every 60 s doubling to 10 min while bob's agent lives, until delivered, until bob's own clock passes its receipt time plus the ask's `deadline_seconds`, or for 24 h when the ask had no deadline; then it ends `unreachable`. The receiver at A dedupes on the reply's message_id, so a retry of a reply that did arrive is not injected twice.

## 9. Failure behaviour

| Case | Sender sees | Receiver side | Recovery |
|---|---|---|---|
| Peer host down or asleep | `unreachable`, reason `timeout` or `unreachable`; `list_agents` `hosts` entry `reachable: false` | Nothing | Send again later; reply obligations retry (section 8) |
| Peer gateway not running | `unreachable/gateway_not_running`, fast | Nothing | Start `remote serve` or `remote enable` there |
| Connection drops after the gateway forwarded | `unknown/pending` | Message may be injected | No automatic resend of an inform; a reply retry is deduped by message_id |
| This host unpaired the peer | `rejected/unpaired`: alias no longer in `peers.json`. A pending reply to that host is not rejected: it stays pending and ends `unreachable/reply_expired` at its limit (R8) | — | Pair again |
| The peer unpaired this host | `rejected/unpaired_by_peer` (handshake refused) | Requests on already-open connections refused at the next request | Pair again |
| Gateway restart mid-ask | Ask stays open on the asker; `GetTask` through the gateway resumes after restart (tasks are journal-backed) | Route keyed by (fingerprint, agent_id) in the journal survives | Nothing to do |
| Peer address changed | `unreachable`; `list_agents` `hosts` entry `reachable: false`; `remote status` names the address it dialled | — | 0.2.0 assumes static IPs (operator ruling): run `remote config --bind` on the moved host, then pair and join again, which updates the address for the same fingerprint. Automatic address refresh by mDNS is 0.2.x |
| This host's bind address vanished | Outbound still works | `remote serve` retries the bind every 10 s; `status` shows `service running, port not bound` | Rejoin the network; a new address means `remote config --bind` and pairing again |
| Port 7745 taken by another program | — | `serve` exits 1 after 60 s; the unit restarts it | `remote config --port` |
| Host key deleted or `--purge` | Peers refuse this host (`tls_pin_mismatch` on their side) | — | Pair again with every peer; `uninstall --purge` prints the list |
| Target name is a native session on the peer | `rejected/remote_native_unsupported`, with a hint that the session has no CrossTalk and cannot be messaged from another host (the peer's listing marks it `messageable: false`) | Nothing injected | Start CrossTalk in that session (its first CrossTalk tool call binds it) |
| Alias renamed on one side | Routes unaffected (keyed by fingerprint) | — | Nothing |

## 10. Spikes before production code

Throwaway code allowed, kept under `docs/spikes/` with a report each; each has a pass line, and a fail stops the slices it gates until the coordinator decides.

| Spike | Size | Pass | Fail means | Gates |
|---|---|---|---|---|
| S1 SDK seam | S (0.5 day) | A gateway terminating pinned mTLS in front of an a2a-go v2.7.0 loopback server delivers a message whose `Sender` carries the host fingerprint from the connection, whatever host header the remote side sends; malformed agent fields are refused before the receiver chain; and well-formed agent and role fields, which the sending host attests and the gateway cannot check, stay inside that host's key namespace and gain no `command` authority (a remote `role: primary` still cannot command) | Rework section 3.2 (identity by header on loopback) before R4 | R4, R5 |
| S2 Real machines | S (0.5 day) | The signed binary on the operator's Mac and the homelab VM, over LAN and tailnet (100.x): `remote serve` (foreground and login unit) accepts, and a shim launched by Claude and by Codex dials out, both directions, with no System Settings change, or the exact macOS Local Network prompt or firewall setting is recorded; sleep and wake recorded (no address-change test: static IPs, operator ruling) | setup.md documents the prompt; if it cannot be passed at all on macOS, stop and ask the operator | R5 (live), R12 |
| S3 Pairing and revocation | S (0.5 day) | Wrong or expired token and a second redemption of one token fail with nothing written; of two concurrent redemptions of one token exactly one enrolls and the other fails with nothing written; after `unpair`, a request on an already-open connection and on a resumed connection is refused | Fix section 7 before R3 | R3 |
| S4 Ask and reply across a disconnect | S (0.5 day) | Two state dirs, two gateways on 127.0.0.1 with different ports: kill the target gateway after injection and before the response; the asker shows `unknown`; the retry injects once (target journal dedupe); the reply reaches the original asker and never a new session that reuses the name | Fix section 8 before R8 | R8 |
| S5 Latency and settle | S (0.25 day) | Send plus the 2 s settle poll (`wire.go:316-339`) ends `accepted` on Wi-Fi and on a relayed tailnet path in 9 of 10 runs, or the settle bound remote targets need is measured | R7 sets a remote settle bound from the measurement | R7 |
| S6 CI two-host job | S (0.5 day, alongside R1-R5) | Two Docker containers on the Ubuntu runner pair through the real CLI and pass one ask and reply in under 3 minutes | The two-gateway 127.0.0.1 test stays the only automated cross-host test; R10 is dropped and said so in the PR | R10 |
| S7 Login unit | S (0.5 day) | On macOS and the Linux VM: `remote enable`, reboot (VM: with no interactive login and linger on), one ask and reply works, `remote disable` leaves no unit; `enable` twice gives identical files; `enable` over a user-edited or foreign unit refuses and writes nothing; `status` reports `service running, port not bound` when the address is taken | Stop and ask the operator: the unit is in 0.2.0 by ruling, so it is not dropped silently | R11 |

S1, S3 and S4 use only 127.0.0.1 and can run in CI-like conditions; S2 and S7 need the operator's machines.

## 11. Implementation slices

Order matters: each slice builds on the one before. Every slice: `go build ./...`, `go test -race ./...`, `go vet ./...`, `gofmt -l .` empty, a Codex review over CrossTalk, and the named tests failing before and passing after. Unit tests run on Linux and macOS in CI. "Two-gateway test" means one `go test` with two state dirs, two config dirs and two gateways on 127.0.0.1 with different ports, harnesses replaced by `adapter.Fake`; it never uses 127.0.0.2 (macOS loopback has only 127.0.0.1).

| Slice | What | Size | DoD and pinning tests | Docs updated |
|---|---|---|---|---|
| R1 Host identity and config | `internal/remote`: key and certificate, fingerprint, `remote.json`, `peers.json` (versioned, atomic, locked, mode-checked, re-read on change); `crosstalk remote config` | M | `TestFingerprintIsSPKISHA256`; `TestPeersHandEditReloaded` (an edited address is used on the next request; a file that does not parse is refused with its path); `TestConfigRefusesWrongMode` (0644 key refused); `TestPeersAtomicUpdateByFingerprint` (re-adding a fingerprint updates, never duplicates); `TestRemoteConfigCLI` (flags, exit codes 0 and 2, interface check) | design §2 (gateway row), §9 |
| R2 Gateway listener and pinning | `crosstalk remote serve`; TLS 1.3; `VerifyConnection` pin; no tickets; per-request recheck; limits; `gateway.token` | M | `TestUnpinnedClientRefusedBeforeHTTP` (no handler runs); `TestTLS12Refused`; `TestUnpairRefusesOpenConnection`; `TestNoSessionResumption` (a second connection performs a full handshake and is re-pinned); `TestGatewayLimits` (body, per-host concurrency, rate); `TestSlowBodyClosed` (a client that sends headers then trickles the body is cut at 10 s); `TestIdleConnectionClosed`; `TestStalledHandshakesBounded` (100 sockets that send no ClientHello, with and without a pending pairing: at most 16 are held, the rest closed at accept, and a pinned peer connects within 6 s); `TestBindRetry` (missing address retried, `status` text); `TestOneGatewayPerHost` (a second `remote serve` exits 1 naming the holder) | design §5 (new "Gateway ingress" paragraph) |
| R3 Pairing | `remote pair`, `remote join`, `POST /v1/pair`, `pending.json` | M | S3's cases as tests: `TestPairWrongSecret`, `TestPairExpired`, `TestPairSingleUse`, `TestPairConcurrentRedeem` (exactly one wins, other writes nothing), `TestPairBindsClientKey` (a MAC made for another fingerprint fails), `TestJoinReadsStdinOnly`, `TestUnenrolledOnlyPair` (unenrolled connection gets 403 on `/v1/agents`); `TestUnenrolledPoolBounded` (a fifth unenrolled connection is refused while 4 stall, and a pinned peer still connects); `TestUnenrolledRefusedWithoutPending` and `TestUnenrolledClosedOnExpiry`; `TestWrongProofsCannotCancelPairing` (many wrong proofs and unknown ids, then the real join succeeds); `TestPairRateLimited` | setup.md "Remote" draft section (not linked from index yet) |
| R4 Remote principal | `receiver.Sender.Host`, `HostAlias`; `key()`, `label()`, `principal()` host-scoped; gateway-credential case in `a2a.authenticate`; receipt and route records use `key` (`receiver.go:267,291`); policy refuses `command` from a remote sender; claim path refuses a remote sender; `askedPeer` and the receipt (`receiver.go:266-267`) compare the host-scoped key; the reply path classifies a remote key as remote, not local-user: `completeReply` (`send.go:583-587`) today marks every non-agent_id key delivered, so until R8 a remote reply obligation stays pending and returns `unreachable/remote_reply_pending`; the inform-route expiry guard (`send.go:520`) applies to remote keys | M | `TestRemoteSenderKeyIsHostScoped`; one forged-field test per field: `TestRemoteCannotPoseAsLocalAgent` (same agent_id as a local agent gets a different key and route), `TestRemoteCannotPoseAsThirdHost`, `TestGatewayHeadersWithoutCredentialIgnored`; `TestRemotePrimaryCannotCommand`; `TestMalformedRemoteFieldsRefused`; `TestRemoteCommandRejected`, `TestRemoteClaimRefused`; `TestRemoteReplyNeverMarkedDeliveredLocally` (a reply to a remote sender is never journaled delivered or returned accepted without a gateway answer); `TestRemoteInformRouteExpires`; existing tests 8, 9, 14, 23 still pass | design §4 (`from` as `name@host`), §5 Authentication, §7 table row "remote" |
| R5 Gateway forwarding | `/v1/a2a/{agent}` for `SendMessage` and `GetTask`; header strip and rebuild; target from the local registry only; method allowlist | M | Two-gateway test `TestTwoHostInform` (inform from host 1 injected once on host 2, sender labelled `name@alias`); `TestGatewayStripsInboundHeaders`; `TestGatewayRefusesOtherMethods` (`CancelTask`, `ListTasks` → `UnsupportedOperation`); `TestGatewayResolvesTargetLocally` (a URL or host in the body is ignored); `TestGatewayUnknownAgent` | design §5, §8 states |
| R6 Remote agent discovery | `GET /v1/agents` on the gateway; parallel bounded fetch in `list_agents`; `host` on rows; `hosts` in the output; no cwd | M | Two-gateway test `TestListAgentsShowsPairedHostAgents` (a session registered on host 2 appears on host 1 as `name@alias` with harness and reachable, and no `cwd`, `pid`, `session_ref` or endpoint key); `TestListAgentsGatewayDown` (`hosts` entry `gateway_not_running`, local rows present, call returns under 1.6 s); `TestListAgentsTimeoutBound` (a peer that accepts and never answers costs at most 1.5 s); `TestRemoteNativeListedUnmessageable` (a native session on host 2 appears on host 1 with `crosstalk: false`, `messageable: false`, a note, and no cwd, pid, session_ref or endpoint key; sending to it is R7's test); `TestUnpairedHostNotListed`; `TestSiteNamesEveryTool` still passes | design §3 Discovery, §8 `list_agents`; how-it-works.md "Remote" draft |
| R7 Addressing and send | `name@host` and `agent_id@host` in `ResolveTarget`; bare names local-only; unknown bare name hint lists `name@host` matches; remote `ask` with deadline; nudge counts the host-scoped key; remote settle bound from S5 | M | `TestBareNameNeverLeavesHost` (a name existing only on host 2 is `no_such_agent` on host 1, with a hint naming `name@alias`); `TestTwoHostAsk` (ask opens a route on host 2, deadline expiry on host 1 with host 2's gateway stopped); `TestUnknownAliasRejected`; `TestLocalNameWithAtStaysLocal` (a local `build@desktop` with `desktop` a paired alias and no remote `build` resolves locally; with a remote `build` too it is `ambiguous_name`); `TestRemoteNameWithSpacesAndUnicode` (a sender named `CrossTalk Remote Plan Worker` or a non-ASCII name arrives intact); `TestRemoteNameReuseBetweenResolveAndPoll` (the poll stays on the resolved agent_id); `TestRemoteNativeTargetRejected` (a `send_message` and an `ask` to a native session's name or agent_id on the peer are `rejected/remote_native_unsupported` with the hint, and host 2's fake adapter records no injection; a name matching nothing is `no_such_agent`); `TestNudgeCountsRemotePeer` | design §8 `to` resolution, §4 |
| R8 Cross-host reply | `completeReply` hands a remote key to a remote branch of `forwardReply` by fingerprint; `retryObligations` replays remote obligations; remote obligation retry timer; ends `unreachable` after deadline or 24 h | M | S4 as `TestTwoHostReplyAcrossDisconnect`; `TestRemoteReplyPendingWhileGatewayDown` (state stays pending, not accepted, in the result and the journal) and `TestRemoteReplyResumesAfterRestart` (restart of the replying shim resends the same reply message_id, injected once); `TestRemoteAskReceiptSuppressesDeadline` (a delivered reply records the receipt, so the asker gets no expiry notice); `TestReplyNeverReachesNameReuser` (new session with the asker's name on host 1 never gets it); `TestReplyRetryAfterPeerWakes` (gateway 1 stopped, then started; the reply arrives once); `TestReplyRouteSurvivesAliasChange`; existing tests 13, 20, 21 still pass | design §8 `reply`, §5 Journal |
| R9 K8 and `remote status` | `install --check` K8 (section 4.4); `remote status` and `--json` | S | `TestK8NotConfiguredPasses`; `TestK8BadModeFails`; `TestK8GatewayDownWarnsWithoutUnit`; `TestRemoteStatusStates` (each of the four gateway states) | setup.md "If something is wrong" |
| R10 CI two-container job | `remote-e2e` job in `ci.yml` on `ubuntu-latest`: two containers, real CLI pairing, one ask and reply with `adapter.Fake`-backed harnesses; not a required check at first | M | The job is green on the PR and fails when pairing is broken (shown once by a deliberately broken commit on the branch, then dropped); runs only on code changes (`release-build.sh ci-scope`) | contributing.md test section |
| R11 Login unit and install option | `remote enable` / `disable`; `install --remote` and the interactive question; `reinstall --remote`; uninstall removes the unit; `--purge` removes `~/.config/crosstalk` | L | `TestEnableIdempotent`; `TestEnableRefusesForeignUnit` (nothing written); `TestDisableRemovesOnlyOwnUnit`; `TestInstallAsksRemoteDefaultNo` (an empty answer writes nothing remote); `TestInstallYesNeverRemote`; `TestInstallRemoteNeedsBindWithYes` (exit 2); `TestUninstallRemovesUnit`; `TestUninstallNamedLastHarnessRemovesUnit`; `TestUninstallKeepsUnitWhileBinaryKept` (another harness or a project registration remains); `TestUninstallLeavesEditedUnit`; `TestPurgeRemovesConfigAndListsPeers`; `TestPurgeKeepsConfigWhileStateKept`; `TestUninstallHomebrewRemovesUnit` (full uninstall with a Homebrew binary: file kept, unit stopped and removed); `TestPurgeRefusesRunningGateway` (foreground gateway and edited-unit gateway); `TestLingerHintPrintedNeverRun` (the command runner never sees `enable-linger`); S7 by hand on both OSes | setup.md "Remote" section final; design §9 Lifecycle |
| R12 Docs and live run | setup.md, how-it-works.md, faq.md ("Does it work across machines?" answered yes, with limits), why.md and index.md same-machine lines; design §1 non-goal removed, §10, §11 tests 34-41 added; a live run on the operator's Mac and homelab VM: one Claude and one Codex session each side, inform, ask and reply both ways, list_agents both ways | M | `TestSiteLinks`, `TestSiteLinksPages` green; a phrase test for the setup.md remote section; live run report under `docs/plans/` | all named |

New design §11 tests added by R12, each pinned by the slice tests above: 34 pinning and unpair on open connections (R2), 35 pairing single use (R3), 36 host-scoped sender identity (R4), 37 two-host inform (R5), 38 remote discovery and its bound (R6), 39 bare names stay local (R7), 40 cross-host reply across a disconnect (R8), 41 the login unit (R11).

Sequencing: spikes S1 and S3 before R2; S4 before R8; S5 before R7; S7 before R11; S2 any time before R12; S6 alongside R1-R5. R1-R9 are serial (each touches the previous slice's code). R10 and R11 can run in parallel after R9. Rough size: about 12 to 16 worker-days including spikes and review rounds; the council's 14 (range 10 to 20) still holds, and it is not a delivery promise.

Release: 0.2.0 tags after R12's live run passes. A final release tag still needs the operator's yes (AGENTS.md Delivery).

## 12. Explicitly out

- Remote native targets: injecting into a non-CrossTalk session on another host (a second injection and lifecycle path).
- Cross-host `command`, `claim`, `release`.
- Any new harness (OpenCode, Antigravity, others): OpenCode spikes may run in parallel, target 0.3.0.
- Relay, store-and-forward while a host is down, multi-hop routing.
- Tailscale identity as authentication (an optional extra admission check may come in 0.2.x, never the only one).
- cwd, paths, pids, session refs or endpoints in remote rows.
- Host discovery and address refresh (0.2.x): mDNS `_crosstalk._tcp` on the LAN and `tailscale status` on a tailnet as pairing suggestions, and an mDNS broadcast that updates the address of an already-paired fingerprint. A suggestion is only a candidate and a broadcast never adds trust; pairing stays required.
- Address-change handling in 0.2.0 beyond a clear `unreachable` and pairing again (static IPs, operator ruling).
- A general `crosstalk config` (view and set settings, edit a paired peer's alias and address): issue #44. In v0.2.0 a peer edit is a fresh pair and join, or a hand edit of `peers.json`.
- Key rotation as a command: delete `~/.config/crosstalk/host.*`, then pair again with every peer.
- Multi-user hosts, other people's machines, cloud sessions.

## 13. Review disposition

Codex reviews over CrossTalk (the standing reviewer, agent_id 8cb20684d21fd580).

Round 1, on 90752ef (7 findings, all checked against the code and fixed):

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | high | Pairing stored sha256(secret) but verified an HMAC keyed by the secret, which a hash cannot do | Fixed: `pending.json` (0600) holds the secret itself until redemption or expiry (sections 6, 7; ADR 0021) |
| 2 | high | Unenrolled clients during pairing could hold all 64 connections with slow bodies; no body or idle deadline | Fixed: whole-request, response and idle deadlines; a separate unenrolled pool of 4, each closed after 15 s, none without a pending pairing (section 3.2); tests in R2 and R3 |
| 3 | high | `completeReply` (`send.go:583-587`) marks every non-agent_id key delivered, so a remote reply would be reported accepted without being sent | Fixed: R4 classifies remote keys and keeps them pending (`remote_reply_pending`) until R8; R8 covers `completeReply`, `forwardReply`, replay and the `send.go:520` route guard; tests that nothing is journaled delivered while the gateway is down and that a restart resumes the same reply |
| 4 | medium | The name grammar `[A-Za-z0-9._-]{1,64}` rejects real harness names with spaces or Unicode | Fixed: names stay any UTF-8 as today, base64url in the header, 256-byte cap, never in a URL path (section 3.2); R7 test |
| 5 | medium | The recipient agent_id was never established, so receipts (`askedPeer`) and `GetTask` polling could miss or follow a reused name | Fixed: the sender resolves the name to an agent_id through `GET /v1/agents` and journals the host-scoped key before sending; paths take an agent_id only (sections 3.2, 8); R4 and R7 tests |
| 6 | medium | Splitting on the last `@` breaks existing local names that contain `@` | Fixed: exact local match first, remote only when the suffix is a paired alias, `ambiguous_name` when both match, aliases may not contain `@` (section 3.4; ADR 0022); R7 test |
| 7 | medium | Uninstall rules for the unit and `--purge` conflicted with how the binary and state steps decide | Fixed: config is purged only with the state directory under the same rule; an edited unit is never removed (section 4.3); R11 tests. The unit rule was revised again in round 2 (#1 below) |

Round 2, on 33162e1 (5 findings, all checked and fixed; the reviewer confirmed the round-1 fixes for 1, 3, 4, 5 and 6):

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | high | "Unit goes when the binary goes" leaves a Homebrew host's gateway running after a full uninstall (Homebrew keeps the file, `install.go:311-313`), and purge could delete the key under a live gateway | Fixed: the unit follows `installKeep`'s no-remaining-user decision, not file deletion; `remote serve` holds `gateway.lock`, and purge refuses while it is held (sections 4.3, 6); R2 and R11 tests |
| 2 | medium | Connections still in the TLS handshake were in no pool, so stalled handshakes were unbounded | Fixed: at most 16 unclassified connections, extra ones closed at accept, slots handed to the pinned or unenrolled pool after the handshake (section 3.2); `TestStalledHandshakesBounded` |
| 3 | medium | Wrong proofs could cancel a legitimate pairing, and a failure could not name its entry | Fixed: the token and request carry an entry id; failures never remove or change an entry; one attempt per second gateway-wide (section 7); R3 tests |
| 4 | medium | `remote_native_unsupported` cannot be produced, since the peer's listing omits native sessions | Fixed then: `no_such_agent` everywhere. Superseded by the operator's ruling after round 4: the listing now includes native rows marked `messageable: false`, so `remote_native_unsupported` is produced from the listing (section 5) |
| 5 | medium | S1 demanded detection of agent and role forgery the trust model cannot detect | Fixed: S1 now checks that the host comes from the connection, malformed fields are refused, and attested fields stay in the host's namespace with no command authority (section 10); R4 tests |

Round 3, on 7828d47 (1 finding; the reviewer confirmed all round-2 fixes and found no further security or code-integration issue):

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | low | S3 said two concurrent redemptions both fail, while section 7 and `TestPairConcurrentRedeem` require exactly one winner | Fixed: S3 now requires one enrollment and no write by the loser (section 10) |

Round 4, on 47fac30: no findings.

After the operator's answers (native rows listed, section 14), round 5 on cdc1c9c (1 finding; the reviewer found no metadata-exposure or delivery-boundary issue in the native-row change):

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | medium | R6's native-row test asserted send and ask rejections, but `name@host` resolution only arrives in R7, so R6 could not pass on its own | Fixed: R6 keeps the row shape and privacy assertions; the send, ask and zero-injection assertions moved to R7's `TestRemoteNativeTargetRejected` (section 11) |

## 14. Open questions for the operator

Answered by the operator (2026-11-06): remote native sessions are listed as rows that cannot be messaged (section 5); `remote pair` and `remote join` stay two commands; a failed S7 goes to the operator (section 10). No questions are open.
