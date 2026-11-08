# Spike 19 — login unit for the gateway (v0.2.0 S7)

- **Date:** 2026-11-08
- **Status:** PARTIAL. The VM unit and linger legs ran; the reboot leg is PENDING-OPERATOR (the session's permission classifier denied `systemctl reboot` on the live inference VM, and a message relayed by another session does not override that); the Mac legs are PENDING-OPERATOR (spike 16); the `remote enable` legs are blocked-on-R11.
- **Plan:** `docs/plans/release-0.2.0.md` section 10 (S7), section 4.2; gates R11.
- **Build:** main `3d7f55b`, ad-hoc signed.
- **Code:** hand-written throwaway units in `docs/spikes/19-live-login-unit/`: `crosstalk-remote.service` (systemd user unit, `__BIN__` is replaced by the binary path) and `dev.getcrosstalk.remote.plist` (launchd agent, `__BIN__` and `__HOME__` replaced). They sit at the plan's section 4.2 file names so `remote status` finds them.

## Pass line (plan row S7)

On macOS and the Linux VM: `remote enable`, reboot (VM: with no interactive login and linger on), one ask and reply works, `remote disable` leaves no unit; `enable` twice gives identical files; `enable` over a user-edited or foreign unit refuses and writes nothing; `status` reports `service running, port not bound` when the address is taken.

Terms: **linger** is systemd's setting that keeps a user's service manager running with no login session; **blocked-on-R11** means the line needs the `remote enable` / `disable` code, which is slice R11 and not built.

## Results

| # | Pass line clause | Result | Evidence |
|---|---|---|---|
| 1 | The unit starts at login on the Mac | PENDING-OPERATOR | Plist written and not loaded: Mac networking is held (spike 16) |
| 2 | VM: linger enabled | PASS | `sudo loginctl enable-linger <user>` then `loginctl show-user ... -p Linger` gives `Linger=yes`; the user manager started with no login |
| 3 | VM: unit installed as a systemd user unit and running | PASS | `systemctl --user enable --now` on three units (the gateway under test, a stand-in peer gateway on a second `HOME`, a fake-harness agent); all `active` |
| 4 | `remote status` sees the unit | PASS | `login unit: installed` at `~/.config/systemd/user/crosstalk-remote.service`; `gateway: running`. `gateway: running` is lock plus TCP connect only (row 10), so inbound acceptance is shown by the ask and reply in spike 16 row 4, not by `status` |
| 5 | VM reboot, unit up afterwards with no interactive login | PENDING-OPERATOR | `sudo systemctl reboot` denied by the permission classifier (live inference VM). Nothing was rebooted. The operator must allow it in the session's terminal |
| 6 | Ask and reply across hosts after the reboot | PENDING-OPERATOR | Depends on row 5 and the Mac. Before the reboot, an ask and reply passed between the two unit-run gateways and the unit-run fake agent on the VM (spike 16 rows 4 and 15; a first run with foreground processes preceded it). Not a reboot test |
| 7 | `remote enable` twice gives identical files | NOT RUN, blocked-on-R11 | |
| 8 | `remote enable` over a user-edited or foreign unit refuses and writes nothing | NOT RUN, blocked-on-R11 | |
| 9 | `remote disable` leaves no unit | NOT RUN, blocked-on-R11 | |
| 10 | `status` reports `service running, port not bound` when the address is taken | FAIL | See "Port held by a foreign listener" |

## Port held by a foreign listener (row 10)

Setup: the gateway unit stopped; a plain TCP listener (Python, not CrossTalk) bound to the gateway's address and port; the unit started.

- The gateway process stayed up and logged `cannot bind <ip>:7745: ... address already in use; retry in 10s`. The unit stayed `active`, so systemd did not restart it (the retry is inside the process, as the plan's section 9 says).
- `crosstalk remote status` printed `gateway: running`, exit 0.
- When the listener was removed the gateway took the port within the 10 s retry and `status` still printed `gateway: running`.

Why: `GatewayStatus` (`internal/remote/status.go`) decides "running" from `gateway.lock` plus a bare TCP connect to `bind:port`. The foreign listener answers the connect, so the check cannot tell it from the gateway. `service running, port not bound` is printed only when nothing at all listens. The plan's pass line, as written, fails against the shipped R9 status. Fix before R11 relies on it: either probe with the pinned TLS handshake (`ProbePeer` already exists) or have the gateway record `bound` in its state, and add a test with a foreign listener.

## What R11 must copy from the hand-written units

systemd user unit as it ran:

```
[Unit]
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=<abs>/crosstalk remote serve
Restart=on-failure
RestartSec=5
[Install]
WantedBy=default.target
```

- `systemctl --user` from a root-run shell needs `XDG_RUNTIME_DIR=/run/user/<uid>`; `enable-linger` creates that manager. Without linger the unit does not start at boot, so the "Linger=no" hint in plan section 4.2 and K8 is needed, not optional.
- The gateway handles an address that is not available itself (10 s retry), so `Restart=on-failure` only covers a real exit such as the 60 s port-held give-up.
- No working directory or environment was needed; the gateway finds its files from `HOME`. A unit for a second `HOME` needs `Environment=HOME=...`.
- launchd: the template uses `RunAtLoad` true, `KeepAlive` `{SuccessfulExit: false}`, both output paths at `~/Library/Logs/crosstalk-remote.log`; not yet loaded, so `launchctl bootstrap gui/<uid>` is unproven here.
- Logs: systemd journal for the unit; launchd log file.

## Left in place for the operator session

On the VM: the throwaway user, `Linger=yes`, the three user units, two gateways (ports 7745 and 7746), the fake-harness agent. On the Mac: the stable-path binary and driver in the session scratch folder, the remote config, host key and certificate under the user's config directory (no peers), and the gateway state files under the user's state directory. No launchd agent is installed.
