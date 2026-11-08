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
	if _, err := remote.ValidateBind(cfg.Bind, false); err != nil {
		rs = append(rs, k8(false, true, "bind address %s is not on any interface of this host", cfg.Bind))
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
	gs, err := remote.GatewayStatus(state, cfg, host.Fingerprint)
	if err != nil {
		return append(rs, k8(false, false, "%v", err))
	}
	switch {
	case gs == remote.StateRunning:
		if len(rs) == 0 {
			rs = append(rs, k8(true, false, "remote configured, gateway answering on %s:%d", cfg.Bind, cfg.Port))
		}
	case uerr == nil:
		rs = append(rs, k8(false, false, "gateway not answering on %s:%d (%s) and the login unit %s exists", cfg.Bind, cfg.Port, gs, unit))
	default:
		rs = append(rs, k8(false, true, "gateway not answering on %s:%d (%s); fine for foreground use", cfg.Bind, cfg.Port, gs))
	}
	return rs
}
