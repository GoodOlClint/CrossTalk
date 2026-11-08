# Spike 17 (S6): CI two-host job

- **Date:** 2026-11-08
- **Base:** main 6db91e4 for the first three runs; main 3d7f55b (R8, PR #77, merged) merged into the branch for the ask and reply runs
- **Code:** `docs/spikes/17-ci-two-host/` (`main.go` workload, `Dockerfile`, `run.sh` scenario) and `.github/workflows/spike-s6.yml` (spike branch and `workflow_dispatch` only)
- **Gates:** plan `docs/plans/release-0.2.0.md` section 10, row S6; precedes slice R10

## Pass line (copied verbatim from the plan, row S6)

Two Docker containers on the Ubuntu runner pair through the real CLI and pass one ask and reply in under 3 minutes

Fail action in the plan: The two-gateway 127.0.0.1 test stays the only automated cross-host test; R10 is dropped and said so in the PR.

Reading used here: the clock starts when `crosstalk remote pair` starts on host A and stops when the reply is injected into host A's Fake harness; image build time is reported separately.

## Verdict

- **PASS.** One ask and reply, pair start to reply received, took 4.1 to 4.3 s in 4 of 4 CI runs on the Ubuntu runner (limit 180 s), with exactly 1 inject of the ask on the receiver and exactly 1 inject (the reply) on the asker in every run. Pairing ran through the real `remote pair` / `remote join` CLI.
- Inform-only leg: 0.76 to 0.91 s in the same 4 runs, and 0.73 to 0.87 s in 3 earlier runs before R8 was on the branch.
- The ask and reply leg was first blocked on R8 (PR #77); it was enabled by default after #77 merged. `run.sh` now exits non-zero if the ask and reply takes 180 s or more.

## Procedure

Each container runs three processes from one image (Debian slim, a non-root user, the real `crosstalk` binary and the spike binary, both built `CGO_ENABLED=0` for Linux on the runner):

1. `crosstalk remote config --bind <container address> --alias <name> --yes`, then `crosstalk remote serve` (the real gateway, in the background).
2. `spike17 agent <name>`: a shim over `adapter.Fake` behind a Codex-shaped harness, registered through its first tool call, with a loopback control port. The real binary has no Fake harness, so this is the only spike-local piece. `spike17 call TOOL JSON` and `spike17 writes` drive it from `docker exec`.
3. Pairing runs only through the real CLI: `remote pair --alias hostb` on A (token on stdout), the token piped to `remote join --alias hosta` on B.

Then A calls `send_message` to `bob@hostb` and the script waits for B's Fake to show one write, waits 3 s more, and requires the count to still be 1. With `REPLY_LEG=1` (the default now), A then calls `ask` to `bob@hostb`, B calls `reply` with the ask's message id (read from B's Fake), and the script waits for the reply on A's Fake and checks both inject counts again.

Network: one user-defined Docker bridge network with a private /24 and static addresses (`docker run --ip`), default port 7745, each container with its own `HOME` and so its own state and config dirs. No production file under `cmd/` or `internal/` was edited; `git diff --stat main` shows only `docs/` and the workflow.

## Versions

Linux x64 runner (`ubuntu-latest`, Ubuntu 24.04 image), Docker 28.0.4, go1.27.3, `actions/checkout@v7`, `actions/setup-go@v7`, `actions/upload-artifact@v5`. Local run: macOS arm64, Docker 29.9.2.

## Results

Pass-line clock is pair start to delivery (inform) or to reply received (ask and reply). `pair` is pair start to `paired with` on A. Times come from the `S6_E2E` line each run prints.

| Run (id) | Trigger | Leg | pair | inform (pair start to 1 inject on B) | ask and reply (pair start to reply on A) | image build | Result |
|---|---|---|---|---|---|---|---|
| 42453410918 | push | both | 0.77 s | 0.91 s | 4.34 s | not recorded | green |
| 42453416272 | dispatch | both | 0.77 s | 0.91 s | 4.34 s | not recorded | green |
| 42453417932 | dispatch | both | 0.73 s | 0.85 s | 4.29 s | not recorded | green |
| 42453419456 | dispatch | both | 0.66 s | 0.76 s | 4.08 s | not recorded | green |
| 42452130115 | push | inform only | 0.72 s | 0.85 s | blocked on R8 | 3.3 s | green |
| 42452255474 | dispatch | inform only | 0.63 s | 0.73 s | blocked on R8 | 6.7 s | green |
| 42452256538 | dispatch | inform only | 0.74 s | 0.87 s | blocked on R8 | 13.2 s | green |
| local, merged tree | local | both | 0.68 s | 0.77 s | 4.1 s | 1.3 s | pass (not CI) |

All four "both" runs are on tip 687771e (R8 merged in, reply leg on). Image build time was not read from the logs of these four runs.

Each whole job (checkout to artifact upload) took 39 to 52 s in the first three runs and 49 to 55 s in the four with the reply leg; the Go build was 12 to 20 s and the scenario 9 to 21 s.

## Surprises for R10

- **Static addresses work.** `docker run --ip` on a user-defined network with an explicit subnet gave stable addresses on the runner, so the plan's static-IP rule needs no workaround. Compose service names were not tried.
- **No production change was needed** to pair through the real CLI in containers. `remote serve` needs only a descriptor in the shared state dir to forward to an agent, so a Fake-backed shim in the same `HOME` is enough; `remote serve` builds its own shim with the real Claude and Codex harnesses and did not mind having neither installed.
- **Startup waits are short**: every run got past the readiness polls (0.2 s interval, 30 s limit) and the whole scenario, including pairing, took 9 to 21 s, so no wait came near the limit. Per-wait times were not recorded.
- **Image build time varies 4x** (3.3, 6.7, 13.2 s) for a two-file `COPY` image, almost all of it pulling the base image. R10 should build with the runner's cache or pull a pinned base once.
- **Token on stdout, hint on stderr**: `pair` prints the token as the first stdout line, so the script reads the first line of its output file; `join` reads the token from stdin, so a pipe works with no tty.
- **What the times are**: elapsed time from pair start to the moment the script observed the delivery. Polling (0.2 s sleep between failed checks) can add delay, so they are observed figures, not a fixed resolution. The ask and reply time also includes the inform leg and its 3 s settle wait that run first, so it overstates the reply path alone; that is the conservative direction for the pass line.
- **`actions/upload-artifact@v5` warns** that it targets Node 20 and is forced to Node 24. `ci.yml` already uses `checkout@v7` and `setup-go@v7`, so the version floor in the kickoff is behind; R10 should pick the current majors when it writes the job.
- **Flakiness:** none in 7 CI runs and 3 local runs. That is too few to bound it.
- **Reply leg**: needs R8 in the build. It is on by default now, and `run.sh` fails the job at 180 s or more. The ask and reply figure is about 3.4 s above the inform figure; `run.sh` sleeps 3 s after the inform before starting the ask, so the ask and reply itself took roughly 0.4 s.

## Gates

`go build ./...`, `go vet ./docs/spikes/17-ci-two-host` and `gofmt -l` are clean for the spike package.
