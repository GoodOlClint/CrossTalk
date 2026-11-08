package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

const remoteStatusUsage = "usage: crosstalk remote status [--json]\n"

type statusPeer struct {
	Alias       string `json:"alias"`
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
	Status      string `json:"status"`
}

type statusReport struct {
	GatewayState string       `json:"gateway_state"`
	Alias        string       `json:"alias"`
	Bind         string       `json:"bind"`
	Port         int          `json:"port"`
	Fingerprint  string       `json:"fingerprint"`
	LoginUnit    string       `json:"login_unit"`
	Peers        []statusPeer `json:"peers"`
}

func remoteUnitPath(home string) string {
	if remoteGOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", "dev.getcrosstalk.remote.plist")
	}
	return filepath.Join(home, ".config", "systemd", "user", "crosstalk-remote.service")
}

func remoteUnitExists(home string) bool {
	_, err := os.Lstat(remoteUnitPath(home))
	return err == nil
}

func runRemoteStatus(args []string, stdout, stderr io.Writer) int {
	asJSON := false
	if len(args) == 1 && args[0] == "--json" {
		asJSON = true
	} else if len(args) > 0 {
		fmt.Fprint(stderr, remoteStatusUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "crosstalk remote status: %v\n", err)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	dir, err := remote.ConfigDir()
	if err != nil {
		return fail(err)
	}
	state, err := registry.DefaultDir()
	if err != nil {
		return fail(err)
	}
	rep := statusReport{LoginUnit: "none", Peers: []statusPeer{}}
	if remoteUnitExists(home) {
		rep.LoginUnit = "installed"
	}
	cfg, err := remote.LoadConfig(dir)
	switch {
	case errors.Is(err, remote.ErrNotConfigured):
		rep.GatewayState = remote.StateNotConfigured
		return printStatus(rep, "", asJSON, stdout, stderr, 0)
	case err != nil:
		return fail(err)
	}
	host, err := remote.LoadHost(dir)
	if err != nil {
		return fail(err)
	}
	peers, err := remote.NewPeers(dir).List()
	if err != nil {
		return fail(err)
	}
	rep.Alias, rep.Bind, rep.Port, rep.Fingerprint = cfg.Alias, cfg.Bind, cfg.Port, host.Fingerprint
	if rep.GatewayState, err = remote.GatewayStatus(state, cfg, host.Fingerprint); err != nil {
		return fail(err)
	}
	rep.Peers = make([]statusPeer, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		rep.Peers[i] = statusPeer{p.Alias, p.Address, p.Fingerprint, ""}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep.Peers[i].Status = remote.ProbePeer(host, p)
		}()
	}
	wg.Wait()
	code := 0
	if rep.LoginUnit != "none" && rep.GatewayState != remote.StateRunning {
		code = 1
	}
	return printStatus(rep, remoteUnitPath(home), asJSON, stdout, stderr, code)
}

func printStatus(r statusReport, unit string, asJSON bool, stdout, stderr io.Writer, code int) int {
	if asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintf(stdout, "%s\n", b)
	} else {
		fmt.Fprintf(stdout, "gateway: %s\n", r.GatewayState)
		if r.Alias != "" {
			fmt.Fprintf(stdout, "alias: %s\nbind: %s\nport: %d\nfingerprint: %s\n", r.Alias, r.Bind, r.Port, r.Fingerprint)
		}
		fmt.Fprintf(stdout, "login unit: %s\n", r.LoginUnit)
		for _, p := range r.Peers {
			fmt.Fprintf(stdout, "peer %s %s %s: %s\n", p.Alias, p.Address, p.Fingerprint, p.Status)
		}
	}
	if code == 1 {
		fmt.Fprintf(stderr, "crosstalk remote status: the gateway is %s but the login unit %s exists\n", r.GatewayState, unit)
	}
	for _, p := range r.Peers {
		if p.Status != "ok" {
			note := ""
			if p.Status == remote.ReasonTimeout {
				note = ": handshake timeout; " + remote.HandshakeHint
			}
			fmt.Fprintf(stderr, "warning: peer %s is %s%s\n", p.Alias, p.Status, note)
		}
	}
	return code
}
