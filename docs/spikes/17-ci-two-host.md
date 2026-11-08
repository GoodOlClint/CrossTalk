# Spike 17 (S6): CI two-host job

- **Date:** 2026-11-08
- **Base:** main 6db91e4 (R1-R7 and R9 merged; R8, PR #77, not merged)
- **Code:** `docs/spikes/17-ci-two-host/` (`main.go` workload, `Dockerfile`, `run.sh` scenario) and `.github/workflows/spike-s6.yml` (spike branch and `workflow_dispatch` only)
- **Gates:** plan `docs/plans/release-0.2.0.md` section 10, row S6; precedes slice R10

## Pass line (copied verbatim from the plan, row S6)

Two Docker containers on the Ubuntu runner pair through the real CLI and pass one ask and reply in under 3 minutes

Fail action in the plan: The two-gateway 127.0.0.1 test stays the only automated cross-host test; R10 is dropped and said so in the PR.

Reading used here: the clock starts when `crosstalk remote pair` starts on host A and stops when the reply is injected into host A's Fake harness; image build time is reported separately.

## Verdict

- **Pairing plus inform leg: PASS in 3 of 3 CI runs**, under 1 s each, exactly 1 inject on the receiving Fake in every run.
- **Ask and reply leg: blocked on R8.** The cross-host reply is slice R8 (PR #77), not on main, so the leg was not run in CI. The scenario code for it is in `run.sh` behind `REPLY_LEG=1` and `workflow_dispatch` input `reply_leg`. It was run once locally against the PR #77 head: one ask and reply in 4.2 s, one inject of the ask on the receiver, one inject of the reply on the asker. That is one local run on an arm64 Docker host, not a CI measurement, and the pass line is not claimed from it.
- Against the pass line as written (an ask and reply under 3 minutes), the overall verdict is therefore **not yet measured in CI**; nothing observed suggests it will come near 180 s.

## Procedure

Each container runs three processes from one image (Debian slim, a non-root user, the real `crosstalk` binary and the spike binary, both built `CGO_ENABLED=0` for Linux on the runner):

1. `crosstalk remote config --bind <container address> --alias <name> --yes`, then `crosstalk remote serve` (the real gateway, in the background).
2. `spike17 agent <name>`: a shim over `adapter.Fake` behind a Codex-shaped harness, registered through its first tool call, with a loopback control port. The real binary has no Fake harness, so this is the only spike-local piece. `spike17 call TOOL JSON` and `spike17 writes` drive it from `docker exec`.
3. Pairing runs only through the real CLI: `remote pair --alias hostb` on A (token on stdout), the token piped to `remote join --alias hosta` on B.

Then A calls `send_message` to `bob@hostb` and the script waits for B's Fake to show one write, waits 3 s more, and requires the count to still be 1. With `REPLY_LEG=1`, A then calls `ask` to `bob@hostb`, B calls `reply` with the ask's message id (read from B's Fake), and the script waits for the reply on A's Fake and checks both inject counts again.

Network: one user-defined Docker bridge network with a private /24 and static addresses (`docker run --ip`), default port 7745, each container with its own `HOME` and so its own state and config dirs. No production file under `cmd/` or `internal/` was edited; `git diff --stat main` shows only `docs/` and the workflow.

## Versions

Linux x64 runner (`ubuntu-latest`, Ubuntu 24.04 image), Docker 28.0.4, go1.27.3, `actions/checkout@v7`, `actions/setup-go@v7`, `actions/upload-artifact@v5`. Local run: macOS arm64, Docker 29.9.2.

## Results

Pass-line clock is pair start to delivery (inform) or to reply received (ask and reply). `pair` is pair start to `paired with` on A. Times come from the `S6_E2E` line each run prints.

| Run (id) | Trigger | pair | inform (pair start to 1 inject on B) | ask and reply | image build | Result |
|---|---|---|---|---|---|---|
| 42452130115 | push | 0.72 s | 0.85 s | blocked on R8 | 3.3 s | green |
| 42452255474 | dispatch | 0.63 s | 0.73 s | blocked on R8 | 6.7 s | green |
| 42452256538 | dispatch | 0.74 s | 0.87 s | blocked on R8 | 13.2 s | green |
| local, PR #77 head, `REPLY_LEG=1` | local | 0.68 s | 0.76 s | 4.2 s | 1.3 s | pass (not CI) |

Each whole job (checkout to artifact upload) took 44 to 54 s, of which the Go build was 12 to 18 s and the scenario 9 to 19 s.

## Surprises for R10

- **Static addresses work.** `docker run --ip` on a user-defined network with an explicit subnet gave stable addresses on the runner, so the plan's static-IP rule needs no workaround. Compose service names were not tried.
- **No production change was needed** to pair through the real CLI in containers. `remote serve` needs only a descriptor in the shared state dir to forward to an agent, so a Fake-backed shim in the same `HOME` is enough; `remote serve` builds its own shim with the real Claude and Codex harnesses and did not mind having neither installed.
- **Startup waits are short**: agent and gateway ready within the first poll in every run. The script polls every 0.2 s up to 30 s; nothing came near the limit.
- **Image build time varies 4x** (3.3, 6.7, 13.2 s) for a two-file `COPY` image, almost all of it pulling the base image. R10 should build with the runner's cache or pull a pinned base once.
- **Token on stdout, hint on stderr**: `pair` prints the token as the first stdout line, so the script reads the first line of its output file; `join` reads the token from stdin, so a pipe works with no tty.
- **Inform timing has a 0.2 s floor** from the poll interval, so sub-second figures are upper bounds of that resolution.
- **`actions/upload-artifact@v5` warns** that it targets Node 20 and is forced to Node 24. `ci.yml` already uses `checkout@v7` and `setup-go@v7`, so the version floor in the kickoff is behind; R10 should pick the current majors when it writes the job.
- **Flakiness:** none in 3 CI runs and 2 local runs. Three runs is too few to bound it.
- **Reply leg**: needs R8 on the branch the job builds. Once #77 is on main, flip the `reply_leg` default and require `reply_seconds` to be a number below 180.

## Gates

`go build ./...`, `go vet ./docs/spikes/17-ci-two-host` and `gofmt -l` are clean for the spike package.
