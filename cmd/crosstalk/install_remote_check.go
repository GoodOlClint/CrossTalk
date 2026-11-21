package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

func installRemoteChecks(home, bin string) []harness.CheckResult {
	dir := filepath.Join(home, ".config", "crosstalk")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	k8 := func(ok, warn bool, f string, a ...any) harness.CheckResult {
		return harness.CheckResult{ID: "K8", OK: ok, WarnOnly: warn, Detail: fmt.Sprintf(f, a...)}
	}
	unit := remoteUnitPath(home)
	unitBytes, uerr := os.ReadFile(unit)
	cfg, err := remote.LoadConfig(dir)
	if errors.Is(err, remote.ErrNotConfigured) {
		if uerr == nil {
			return []harness.CheckResult{k8(false, false, "login unit %s exists but remote is not configured", unit)}
		}
		return []harness.CheckResult{k8(true, false, "remote not configured")}
	}
	if err != nil {
		return []harness.CheckResult{k8(false, false, "%v", err)}
	}
	var rs []harness.CheckResult
	host, err := remote.LoadHost(dir)
	if err != nil {
		rs = append(rs, k8(false, false, "%v", err))
	}
	if _, err := remote.NewPeers(dir).List(); err != nil {
		rs = append(rs, k8(false, false, "%v", err))
	}
	bindErr := error(nil)
	if !remote.IsWildcard(cfg.Bind) {
		_, bindErr = remote.ValidateBind(cfg.Bind, false)
	}
	if uerr == nil {
		switch {
		case cfg.UnitSHA256 != sha256Hex(unitBytes):
			rs = append(rs, k8(false, false, "login unit %s is not the file CrossTalk wrote (its hash differs from unit_sha256 in remote.json); uninstall will leave it", unit))
		case !bytes.Equal(unitBytes, unitContent(remoteGOOS, bin, home)):
			rs = append(rs, k8(false, false, "login unit %s runs a binary other than %s; run crosstalk reinstall --remote or crosstalk remote enable", unit, bin))
		case lingerOff(context.Background()):
			rs = append(rs, k8(false, true, "login unit %s starts at login only while Linger is off; %s", unit, lingerHint()))
		}
	}
	gs, probe, err := remote.GatewayStatusWhy(state, cfg, host.Fingerprint)
	shown := gs
	if gs == remote.StateNoPort {
		shown = noPortText(probe, uerr == nil)
	}
	if err != nil {
		return append(rs, k8(false, false, "%v", err))
	}
	if bindErr != nil && gs != remote.StateWaitingAddr {
		rs = append(rs, k8(false, true, "bind address %s is not on any interface of this host", cfg.Bind))
	}
	switch {
	case gs == remote.StateWaitingAddr:
		rs = append(rs, k8(false, true, "gateway is waiting for its bind address %s, which is not on this host; it recovers by itself when the address returns; to use another address now run: crosstalk remote config --bind <current address>, or to listen on all addresses run: crosstalk remote config --bind all --advertise <tailnet address or name>, then restart the gateway (stop and rerun crosstalk remote serve, or run crosstalk remote disable then crosstalk remote enable)", cfg.Bind))
	case gs == remote.StateRunning:
		if len(rs) == 0 {
			rs = append(rs, k8(true, false, "remote configured, gateway answering on %s", cfg.ListenLabel()))
		}
	case uerr == nil && gs == remote.StateNotRunning:
		rs = append(rs, k8(false, false, "gateway not answering on %s (%s) and the login unit %s exists", cfg.ListenLabel(), shown, unit))
	case uerr == nil:
		rs = append(rs, k8(false, true, "gateway process runs but is not answering on %s (%s)", cfg.ListenLabel(), shown))
	default:
		rs = append(rs, k8(false, true, "gateway not answering on %s (%s); fine for foreground use", cfg.ListenLabel(), shown))
	}
	return rs
}

// noPortText is the K8 wording for a gateway whose lock is held but whose
// listener did not answer as this host's gateway. A refused dial keeps the
// state string; a handshake or certificate failure says so and names a
// gateway left running on a previous version as a likely cause.
func noPortText(p remote.ListenerProbe, unit bool) string {
	restart := "stop it and rerun crosstalk remote serve"
	if unit {
		restart = gatewayRestartLine()
	}
	switch p {
	case remote.ProbeHandshakeFailed:
		return "service running, handshake failed: the port accepts connections but no TLS 1.3 certificate came back; a gateway still running a previous CrossTalk version is a likely cause, restart it: " + restart
	case remote.ProbeMismatch:
		return "service running, certificate mismatch: the port presents a certificate other than this host's host.crt; another program on the port, or a gateway still running a previous CrossTalk version, is a likely cause; restart the gateway: " + restart
	}
	return remote.StateNoPort
}
