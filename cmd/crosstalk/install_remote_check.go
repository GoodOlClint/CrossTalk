package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

func installRemoteChecks(home string) []harness.CheckResult {
	dir := filepath.Join(home, ".config", "crosstalk")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	k8 := func(ok, warn bool, f string, a ...any) harness.CheckResult {
		return harness.CheckResult{ID: "K8", OK: ok, WarnOnly: warn, Detail: fmt.Sprintf(f, a...)}
	}
	cfg, err := remote.LoadConfig(dir)
	if errors.Is(err, remote.ErrNotConfigured) {
		return []harness.CheckResult{k8(true, false, "remote not configured")}
	}
	if err != nil {
		return []harness.CheckResult{k8(false, false, "%v", err)}
	}
	var rs []harness.CheckResult
	if _, err := remote.LoadHost(dir); err != nil {
		rs = append(rs, k8(false, false, "%v", err))
	}
	if _, err := remote.NewPeers(dir).List(); err != nil {
		rs = append(rs, k8(false, false, "%v", err))
	}
	if _, err := remote.ValidateBind(cfg.Bind, false); err != nil {
		rs = append(rs, k8(false, true, "bind address %s is not on any interface of this host", cfg.Bind))
	}
	gs, err := remote.GatewayStatus(state, cfg)
	if err != nil {
		return append(rs, k8(false, false, "%v", err))
	}
	switch unit := remoteUnitPath(home); {
	case gs == remote.StateRunning:
		if len(rs) == 0 {
			rs = append(rs, k8(true, false, "remote configured, gateway answering on %s:%d", cfg.Bind, cfg.Port))
		}
	case remoteUnitExists(home):
		rs = append(rs, k8(false, false, "gateway not answering on %s:%d (%s) and the login unit %s exists", cfg.Bind, cfg.Port, gs, unit))
	default:
		rs = append(rs, k8(false, true, "gateway not answering on %s:%d (%s); fine for foreground use", cfg.Bind, cfg.Port, gs))
	}
	return rs
}
