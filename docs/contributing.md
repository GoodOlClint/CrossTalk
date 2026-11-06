# Contributing

CrossTalk is a Go program in one repository: https://github.com/GoodOlClint/CrossTalk. The full contributor guide is [CONTRIBUTING.md](https://github.com/GoodOlClint/CrossTalk/blob/main/CONTRIBUTING.md) in the repository root. This page is the short version.

## Get going

You need Go (the version in `go.mod`).

```sh
git clone https://github.com/GoodOlClint/CrossTalk.git
cd CrossTalk
go build ./...
go test ./...
```

Before you open a pull request, run the same checks CI runs:

```sh
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
```

## Where things are

- `cmd/crosstalk`: the command line (`mcp`, `install`, `uninstall`, `reinstall`, `send`, `capture`, `version`).
- `internal/mcpserver`: the seven tools the model sees.
- `internal/shim`, `internal/receiver`, `internal/a2a`, `internal/udsingress`: the shim, the one receiver chain every inbound message passes through, and the two ingress points.
- `internal/harness/claude`, `internal/harness/codex`: everything specific to one harness.
- `internal/journal`, `internal/registry`: state on disk.
- `plugins/crosstalk`: the plugin and the skill that teaches a model to use the tools.
- `docs`: this site. `docs/design.md` is the specification and `docs/decisions` holds the decision records.

## Rules that matter

- One receiver chain. Every inbound message goes through authenticate, dedupe, hop check, policy, inject. Do not inject from an ingress handler.
- Harness code stays in the harness package. The shim, the MCP server and the A2A code import no harness package. A new harness is one package plus one line in `cmd/crosstalk`.
- Write the failing test first. Show that it fails, then make it pass.
- Record decisions. An architectural choice goes in `docs/decisions/` as a short record (context, decision, rejected alternatives, consequences). Every record is provisional and is edited in place when the code moves.

## Add a harness

Implement the `harness.Harness` interface in `internal/harness/harness.go` in a new package: identify the calling session, bind it, discover sessions, inject a message, and render an inbound message for that harness's model. Look at `internal/harness/claude` and `internal/harness/codex` for two different shapes. The design reasons are in [ADR 0008](decisions/0008-each-harness-is-one-package-behind-a-shared-interface.md).

## Pull requests

`main` is protected. Every change goes through a pull request, and the checks `test (ubuntu-latest)` and `test (macos-latest)` must pass. A change that touches only Markdown, `docs/` or the two marketplace files runs a short set of docs tests instead of the full suite. Keep a pull request to one change and say in it what you ran.

## Good first changes

- Fix a typo or unclear sentence on any page of this site. Run `go test ./cmd/crosstalk -run 'TestSite'` to check the links.
- Read the [FAQ](faq.md) and the [setup page](setup.md) as a newcomer and file an issue for anything that did not work.
- Pick an open issue labeled `good first issue`, if there is one.
