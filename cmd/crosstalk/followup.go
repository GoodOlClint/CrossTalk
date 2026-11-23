package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/quiesce"
	"github.com/GoodOlClint/crosstalk/internal/refresh"
	"github.com/GoodOlClint/crosstalk/internal/remote"
	"github.com/GoodOlClint/crosstalk/internal/upgrade"
)

// followUpSettle bounds the wait for a restarted gateway to take gateway.lock.
var followUpSettle = 10 * time.Second

const (
	followUpStepTimeout = 5 * time.Minute
	followUpInterval    = 15 * time.Minute
	followUpBound       = 6 * time.Hour
)

func installMarketplace() string {
	return cmp.Or(os.Getenv("CROSSTALK_MARKETPLACE"), "GoodOlClint/CrossTalk")
}

var errNoHandshake = errors.New("the upgrade follow-up starts no registered command")

// followUpEnv is the install environment of the follow-up: harness commands
// run without a terminal and the K6 handshake is refused.
func followUpEnv(bin, home, stateDir string) harness.InstallEnv {
	return harness.InstallEnv{
		Bin: bin, Version: mcpserver.Version, Home: home, StateDir: stateDir,
		Run:         installRun,
		Handshake:   func(context.Context, string, []string, []string) error { return errNoHandshake },
		Marketplace: installMarketplace(),
		Out:         io.Discard,
	}
}

// gatewayOnBin waits up to wait for the process holding gateway.lock to run
// bin.
func gatewayOnBin(ctx context.Context, stateDir, bin string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		held, err := remote.GatewayLockHeld(stateDir)
		var why error
		switch pid, perr := strconv.Atoi(remote.GatewayHolderPID(stateDir)); {
		case err != nil:
			why = err
		case !held || perr != nil:
			why = errors.New("no gateway is running")
		default:
			runs, known := gatewayRunsBin(pid, bin)
			switch {
			case runs && known:
				return nil
			case !known:
				why = fmt.Errorf("could not tell which binary the gateway (pid %d) runs", pid)
			default:
				why = fmt.Errorf("the gateway (pid %d) does not run %s", pid, bin)
			}
		}
		if !time.Now().Before(deadline) {
			return why
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func unitOwned(home, cfgDir string) bool {
	_, _, present, owned, _ := unitOwnership(home, cfgDir)
	return present && owned
}

func gatewayTarget(env harness.InstallEnv, cfgDir string) upgrade.Target {
	return upgrade.Target{
		Name:     "gateway",
		Eligible: func() bool { return unitOwned(env.Home, cfgDir) },
		Run: func(ctx context.Context, w io.Writer) error {
			held, err := remote.GatewayLockHeld(env.StateDir)
			if err != nil {
				return err
			}
			pid, perr := strconv.Atoi(remote.GatewayHolderPID(env.StateDir))
			if !held || perr != nil {
				return nil
			}
			runs, known := gatewayRunsBin(pid, env.Bin)
			if !known {
				return fmt.Errorf("could not tell which binary the gateway (pid %d) runs; not restarting it unattended", pid)
			}
			if runs {
				return nil
			}
			return gatewayRestartStep(env.Home, cfgDir, env.StateDir, env.Bin, w).Apply(ctx)
		},
		Verify: func(ctx context.Context) error { return gatewayOnBin(ctx, env.StateDir, env.Bin, followUpSettle) },
	}
}

// followUpTargets are the harnesses that are found, registered to run env.Bin
// (which implies set up) and can update their plugin alone, then the gateway
// when CrossTalk's login unit exists.
func followUpTargets(env harness.InstallEnv, hs []harness.Harness, cfgDir string) []upgrade.Target {
	var ts []upgrade.Target
	for _, h := range hs {
		in, ok := h.(harness.Installer)
		pu, ok2 := h.(harness.PluginUpdater)
		if !ok || !ok2 {
			continue
		}
		eligible := func() bool {
			if !in.Found() {
				return false
			}
			reg, _ := in.Registered(env)
			return reg
		}
		if !eligible() {
			continue
		}
		step := pu.PluginStep(env)
		ts = append(ts, upgrade.Target{
			Name:     string(h.Name()),
			Eligible: eligible,
			Run: func(ctx context.Context, _ io.Writer) error {
				err := step.Apply(ctx)
				if errors.Is(err, harness.ErrMarketplaceMismatch) {
					return fmt.Errorf("%w: %w", upgrade.ErrNotRetryable, err)
				}
				return err
			},
			Verify: func(ctx context.Context) error {
				if r := pu.PluginCheck(ctx, env); !r.OK {
					return errors.New(r.Detail)
				}
				if reg, err := in.Registered(env); err != nil || !reg {
					return fmt.Errorf("the %s registration no longer runs %s after the plugin step (%v)", h.Name(), env.Bin, err)
				}
				return nil
			},
		})
	}
	if unitOwned(env.Home, cfgDir) {
		ts = append(ts, gatewayTarget(env, cfgDir))
	}
	return ts
}

func followUpRunner(env harness.InstallEnv, hs []harness.Harness, cfgDir string, notice func(string)) *upgrade.Runner {
	return &upgrade.Runner{
		StateDir: env.StateDir, Version: env.Version, Notice: notice,
		Targets:  followUpTargets(env, hs, cfgDir),
		Interval: followUpInterval, Bound: followUpBound,
		TargetTimeout: followUpStepTimeout,
		Enter:         quiesce.Hold,
	}
}

// startFollowUp runs the upgrade follow-up in the background when refresh is
// armed and not turned off (ADR 0031), once bound closes: a shim that serves
// no session, such as a `claude mcp list` probe, never starts it. stop cancels
// it and waits for it.
func startFollowUp(ctx context.Context, armed bool, bound <-chan struct{}, env harness.InstallEnv, hs []harness.Harness, cfgDir string, notice func(string)) (stop func(), started bool) {
	if !armed || refresh.Off() {
		return func() {}, false
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			return
		case <-bound:
		}
		followUpRunner(env, hs, cfgDir, notice).Run(ctx)
	}()
	return func() { cancel(); <-done }, true
}
