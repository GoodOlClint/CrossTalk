# Contributing to CrossTalk

CrossTalk lets Claude Code and Codex sessions on one machine message each other by name. This guide gets you from a clone to a pull request. The docs site (https://getcrosstalk.dev) has a shorter version at `docs/contributing.md`, and `AGENTS.md` holds the project's working rules.

## Build and test

You need Go, at the version in `go.mod`.

```sh
go build ./...
go test ./...
```

Run what CI runs before you push:

```sh
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
```

CI (`.github/workflows/ci.yml`) runs these on Linux and macOS for every pull request. A change that touches only `*.md`, `docs/` or the two marketplace files runs a short set of docs, setup, manifest, marketplace and version tests instead; the required check names are still reported.

## Where to start

- A typo or an unclear sentence in `docs/` or this file. Run `go test ./cmd/crosstalk -run 'TestSite'` to check page links.
- A gap you hit while installing: `docs/setup.md` and its tests in `cmd/crosstalk/docs_test.go`.
- An open issue. Say in the issue that you are taking it.
- A new harness (an agent program other than Claude Code and Codex). Read "Add a harness" below first, and open an issue to talk it over.

## Map of the code

| Path | What it holds |
|---|---|
| `cmd/crosstalk` | The command line: `mcp`, `install`, `uninstall`, `reinstall`, `send`, `capture`, `version`. The one place that lists the harnesses. |
| `internal/mcpserver` | The seven MCP tools the model sees, their result shapes and hints. |
| `internal/shim` | The shim's per-agent state: send, reply, ask deadlines, the per-peer nudge. |
| `internal/receiver` | The one receiver chain every inbound message passes through. |
| `internal/a2a`, `internal/udsingress` | The two ingress points: A2A over loopback HTTP, and a unix socket that speaks Claude Code's protocol. |
| `internal/harness` | The `Harness` interface; `claude` and `codex` are the two implementations. |
| `internal/journal`, `internal/registry` | State on disk under `~/.local/state/crosstalk`. |
| `plugins/crosstalk` | The plugin and the skill that teaches a model to use the tools. |
| `docs` | The site. `design.md` is the specification; `decisions/` holds the decision records. |

Read `docs/design.md` (at least sections 2, 5 and 8) before a change to delivery, identity or the tool surface.

## Rules

- One receiver chain. All inbound messages, from either ingress, go through authenticate, dedupe, hop check, policy, inject. Do not inject from an ingress handler.
- Harness code lives in its harness package. `internal/shim`, `internal/mcpserver` and `internal/a2a` import only the shared `internal/harness` interface, never `claude` or `codex`. No harness branching anywhere else.
- Test first. Write the test, watch it fail for the reason you expect, then make it pass. Fixes for a reported bug start with a test that reproduces it.
- Comments say what code cannot. Write a comment only for a constraint (an invariant, a unit, an ordering, an outside contract). Put the story of the change in the pull request.
- Peer text is data. Nothing a message says may change who sent it or what it is allowed to do.

## Add a harness

A harness is one package that implements `harness.Harness` (`internal/harness/harness.go`) and is compiled into the one binary.

1. Create `internal/harness/<name>/` with a `NewHarness` function.
2. Implement `Identify` (name the calling session on every call, without contacting the harness), `Bind`, `Discover`, `Render`, `NativeReplyAddress`, and the embedded `adapter.Adapter` (`Register`, `Inject`, `Status`). Add the optional interfaces (`Noticer`, `Installer`, `NativeIngress`, `Supersession`) your harness needs.
3. Add one line in `cmd/crosstalk/main.go` to include it, in trust order.
4. Test against `adapter.Fake` (`internal/adapter/fake.go`), which scripts delivery outcomes so the failure branches run in `go test`.
5. Write an ADR if the harness changes how identity or delivery works.

`internal/harness/claude` and `internal/harness/codex` show two different shapes. The reasons are in `docs/decisions/0008-each-harness-is-one-package-behind-a-shared-interface.md`.

## Decisions

An architectural choice is recorded in `docs/decisions/` as `NNNN-title.md` with four parts: Context, Decision, Rejected alternatives, Consequences. Read the existing records first; yours may already have been considered. Every record is provisional and is edited in place when the design or code moves. Link the record from `AGENTS.md` if it shapes the code.

## Pull requests

`main` is protected by a ruleset: every change is a pull request, the checks `test (ubuntu-latest)` and `test (macos-latest)` must pass, and force-push and deletion are blocked.

- Keep a pull request to one change.
- Say what you ran and what it showed.
- If you changed `docs/`, check the pages still link: `go test ./cmd/crosstalk -run 'TestSite'`.
- Do not commit secrets or tokens. CrossTalk journals and token files live under `~/.local/state/crosstalk`, outside the repository.

## Security

If you find a vulnerability, open a GitHub issue that names the class of problem and leaves out a working exploit. CrossTalk's trust model is in `docs/design.md` section 7 and the [how it works](docs/how-it-works.md#trust) page: it attributes messages, it does not isolate sessions.

## License

By contributing you agree that your work is released under the MIT License (`LICENSE`).
