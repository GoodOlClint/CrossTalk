# ADR 0031 — The first shim on a new version runs the plugin and gateway steps of an upgrade

- **Status:** Proposed
- **Date:** 2026-11-21
- **Deciders:** operator (interview 2026-11-21, Q3 and Q4) + Claude (Opus 5.5)
- **Context source:** `docs/plans/upgrade-refresh.md`; ADR 0016, 0018; spike `docs/spikes/20-upgrade-refresh.md`

## Context

The operator asked that a Homebrew upgrade run the CrossTalk install commands. After `brew upgrade crosstalk`, the registered path `<prefix>/bin/crosstalk` does not change (ADR 0018), so registration, allow rules, PATH and the Codex config stay correct. Two things go stale: the plugin (the skill), pinned to the old release tag (K1), and the remote gateway, if its login unit exists, which keeps the old binary. Today the formula's caveats ask the user to run `crosstalk install` again.

Homebrew cannot do it. `post_install` runs in Homebrew's sandbox (`Sandbox.run_or_fork` in `formula_installer.rb` `post_install`, rules from `add_install_hook_rules` in `sandbox.rb`): it may write the keg, the prefix link directories, temp, cache and logs; home is denied for read, and `.claude` and `.claude.json` are named in the denied list; network is denied unless the formula allows it (Homebrew 7.0.15, spike 20). Homebrew has no user hook that runs after an upgrade outside the sandbox. ADR 0018 already rejected `post_install`.

ADR 0016 makes each install step confirmed on a terminal, or `--yes`. ADR 0006 says the shim spawns nothing at runtime; install running harness CLIs is setup. With ADR 0030, every shim notices a new binary within seconds, outside any sandbox, as the user.

## Decision

- When a shim with refresh armed (ADR 0030) starts or resumes on a version for which a target's marker `~/.local/state/crosstalk/upgrade/<version>/<target>.done` is missing (one target per found and set-up harness, plus `gateway` when the owned login unit exists), it tries a non-blocking lock on `~/.local/state/crosstalk/upgrade.lock`. The one shim that gets it reads the markers again under the lock and runs what is still missing in the background; every other shim skips. `crosstalk install` takes the same lock (blocking) and writes the markers only for the targets it installed and verified.
- The follow-up runs only the existing steps, without a terminal (operator Q4): each pending harness's plugin step, then K1 (the installed plugin is at the binary's version); `gatewayRestartStep` when the gateway runs another binary, then a check that the gateway holding `gateway.lock` runs the installed binary. It never edits settings, permission rules, PATH, MCP registration or the Codex config, and runs no other check: K6 starts the registered command and K7 a login shell, which stay for a manual install.
- A target's marker is written only when its step and its verification passed. A failed target is retried at the next shim start on that version and by the lock holder every 15 minutes for 6 hours. The run is logged to `upgrade/<version>/follow-up.log`, and a one-time notice goes on the next tool result of the shim's session (the held-notice path, ADR 0006 #151): what was updated, or the failed target and "run `crosstalk install`".
- The same `CROSSTALK_NO_AUTO_REFRESH=1` that turns off ADR 0030 turns this off.
- The formula's caveats change to say the plugin update runs on its own when a session next starts, and that `crosstalk install` remains the manual path.

## Rejected alternatives

- **`post_install` running `crosstalk install`.** Sandboxed: cannot read `~/.claude` or write home (measured; ADR 0018 rejected it for the confirmation rule too).
- **A launchd or systemd path watcher that runs `crosstalk install --yes`.** Adds a unit to install, check and uninstall on two platforms, and `WatchPaths` on a Homebrew symlink is not measured; the shims already see the change.
- **Full `crosstalk install --yes` from the shim.** Edits settings and rules with no one at a terminal; the operator limited it to the plugin and the gateway (Q4).
- **Caveats text only.** Today's behaviour; the operator asked for more.

## Consequences

- Amends ADR 0006: a shim may run the harness CLIs and the login-unit command for this follow-up. It still starts no messaging process.
- Amends ADR 0016: the plugin and gateway steps may run unconfirmed in this follow-up, and install takes the upgrade lock and writes the marker.
- Amends ADR 0018: the caveats and the "plugin stays at the old version until the user runs `crosstalk install`" consequence.
- A `~/.local/bin` upgrade is always done by `crosstalk install`, which writes the marker, so the follow-up matters for Homebrew and for a copy made by hand.
- If the session whose shim holds the lock ends during the follow-up, the lock is released and the unfinished markers are not written; the next shim to start on that version runs them again. Both steps are idempotent (ADR 0016).
- ADR 0019 lands the marketplace ref before the formula moves, so the K1 check mostly guards a stale local marketplace cache.
- The harness CLIs need network for the marketplace update; offline, the step fails, the marker is not written, and the next shim start retries.
