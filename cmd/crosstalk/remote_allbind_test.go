package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/remote"
)

func tailnetInterface(t *testing.T) {
	t.Helper()
	old := remote.InterfaceAddrs
	remote.InterfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("100.101.102.103").To4(), Mask: net.CIDRMask(32, 32)}}, nil
	}
	t.Cleanup(func() { remote.InterfaceAddrs = old })
}

func TestRemoteConfigAllAddresses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "crosstalk")
	tailnetInterface(t)

	code, _, errs := remoteRun(t, false, "config", "--bind", "all", "--yes")
	if code != 2 || !strings.Contains(errs, "--advertise") || !strings.Contains(errs, "suggestion: --advertise 100.101.102.103") {
		t.Fatalf("all without advertise: %d %q, want 2, the need and the tailnet suggestion", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "remote.json")); err == nil {
		t.Fatal("a refused command wrote remote.json, or applied the suggestion")
	}
	for _, lit := range []string{"0.0.0.0", "::"} {
		if code, _, _ := remoteRun(t, false, "config", "--bind", lit, "--advertise", "box.ts.net", "--force", "--yes"); code != 2 {
			t.Errorf("--bind %s: exit %d, want 2 (a literal wildcard stays refused)", lit, code)
		}
	}

	code, out, errs := remoteRun(t, false, "config", "--bind", "all", "--advertise", "box.ts.net", "--yes")
	if code != 0 || !strings.Contains(out, "advertise:   box.ts.net") {
		t.Fatalf("all with advertise: %d %q %q", code, out, errs)
	}
	c, err := remote.LoadConfig(dir)
	if err != nil || !remote.IsWildcard(c.Bind) || c.AdvertiseAddr() != "box.ts.net" {
		t.Fatalf("stored %+v %v", c, err)
	}

	if code, _, errs := remoteRun(t, false, "config", "--advertise", "0.0.0.0", "--yes"); code != 2 {
		t.Errorf("wildcard advertise in all-addresses mode: %d %q", code, errs)
	}
	if code, out, errs := remoteRun(t, false, "config", "--bind", "0.0.0.0", "--yes"); code != 2 {
		t.Errorf("wildcard literal on an all-addresses config: %d %q %q", code, out, errs)
	}
	if code, _, errs := remoteRun(t, false, "config", "--bind", "203.0.113.9", "--yes"); code != 2 {
		t.Errorf("back to single mode with an address not on this host: %d %q", code, errs)
	}
	if code, _, errs := remoteRun(t, false, "config", "--bind", "100.101.102.103", "--yes"); code != 0 {
		t.Errorf("back to single mode: %d %q", code, errs)
	}
	if c, _ := remote.LoadConfig(dir); remote.IsWildcard(c.Bind) || c.Bind != "100.101.102.103" {
		t.Errorf("after switching back: %+v", c)
	}
}

func TestRemoteConfigAllAddressesNoSuggestionWithoutTailnet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := remote.InterfaceAddrs
	remote.InterfaceAddrs = func() ([]net.Addr, error) { return nil, nil }
	t.Cleanup(func() { remote.InterfaceAddrs = old })
	_, _, errs := remoteRun(t, false, "config", "--bind", "all", "--yes")
	if strings.Contains(errs, "suggestion") {
		t.Fatalf("suggestion with no tailnet address: %q", errs)
	}
}

func allAddressesHome(t *testing.T) (home, dir, state string) {
	t.Helper()
	home, dir, state = statusHome(t, true)
	c, err := remote.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Bind, c.Advertise = remote.AllBind, "box.ts.net"
	if err := remote.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	return home, dir, state
}

func TestRemotePairAndStatusCarryAdvertiseNotWildcard(t *testing.T) {
	home, dir, state := allAddressesHome(t)
	_ = home
	host, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := remote.LoadConfig(dir)
	gw := remote.NewGateway(remote.GatewayConfig{StateDir: state, Host: host, Bind: cfg.Bind, Port: cfg.Port, Alias: "desk", Peers: remote.NewPeers(dir)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-gw.Ready():
	case err := <-done:
		t.Fatal(err)
	}

	var out, errb syncBuf
	exit := make(chan int, 1)
	go func() {
		exit <- runRemote([]string{"pair", "--alias", "laptop"}, strings.NewReader(""), false, &out, &errb)
	}()
	var tok remote.PairToken
	for deadline := time.Now().Add(5 * time.Second); ; {
		line, _, _ := strings.Cut(out.String(), "\n")
		if t2, err := remote.ParsePairToken(line); err == nil {
			tok = t2
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no token printed: out %q err %q", out.String(), errb.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if tok.Address != "box.ts.net" {
		t.Fatalf("token address %q, want the advertise value", tok.Address)
	}
	if strings.Contains(out.String(), "0.0.0.0") || strings.Contains(out.String(), "::") {
		t.Fatalf("pair output names a wildcard: %q", out.String())
	}
	tok.Address, tok.Port = "127.0.0.1", gw.Addr().(*net.TCPAddr).Port
	bdir := filepath.Join(t.TempDir(), "crosstalk")
	bhost, err := remote.LoadOrCreateHost(bdir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.JoinGateway(context.Background(), remote.JoinOptions{
		Host: bhost, Config: remote.Config{Version: 1, Alias: "beta", Bind: "127.0.0.1", Port: 7746}, Peers: remote.NewPeers(bdir), Token: tok, Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("join: %v", err)
	}
	select {
	case <-exit:
	case <-time.After(5 * time.Second):
		t.Fatal("remote pair did not notice the redemption")
	}

	_, obj, _ := statusJSON(t)
	var adv, bind string
	json.Unmarshal(obj["advertise"], &adv)
	json.Unmarshal(obj["bind"], &bind)
	if adv != "box.ts.net" || !remote.IsWildcard(bind) {
		t.Fatalf("status advertise %q bind %q", adv, bind)
	}
}

func TestStatusAndK8ForWildcardBind(t *testing.T) {
	home, dir, state := allAddressesHome(t)
	statusUnit(t, home)
	noInterfaceAddrs(t)

	cfg, _ := remote.LoadConfig(dir)
	host, _ := remote.LoadHost(dir)
	statusLock(t, state)
	if st, err := remote.GatewayStatus(state, cfg, host.Fingerprint); err != nil || st == remote.StateWaitingAddr {
		t.Fatalf("a wildcard bind with no listener reads %q (%v), want a no-port state, never waiting for address", st, err)
	}
	failed, out := statusChecks(t, home)
	if strings.Contains(out, "not on any interface") || strings.Contains(out, "waiting for its bind address") {
		t.Fatalf("K8 treats a wildcard bind as a missing address:\n%s", out)
	}
	if failed {
		t.Fatalf("K8 failed with a running gateway process:\n%s", out)
	}
	ls := k8Lines(out)
	if len(ls) != 1 || !strings.Contains(ls[0], "all addresses") || strings.Contains(ls[0], "[::]") {
		t.Fatalf("K8 lines %q, want one naming all addresses", ls)
	}
}

func TestK8WaitingWarnNamesAllAddressesMode(t *testing.T) {
	home, _, state := statusHome(t, true)
	statusUnit(t, home)
	statusLock(t, state)
	noInterfaceAddrs(t)
	_, out := statusChecks(t, home)
	if !strings.Contains(out, "--bind all --advertise") {
		t.Fatalf("K8 warning does not name all-addresses mode:\n%s", out)
	}
}

func TestEnableNarrowingFromAllAddressesRestartsUnit(t *testing.T) {
	eachOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos)
		mustEnable(t, freePort(t))
		dir := filepath.Join(home, ".config", "crosstalk")
		c, err := remote.LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		c.Bind, c.Advertise = remote.AllBind, "box.ts.net"
		if err := remote.SaveConfig(dir, c); err != nil {
			t.Fatal(err)
		}
		if code, out := enableCmd(t, "--bind", "127.0.0.1", "--yes"); code != 0 {
			t.Fatalf("narrowing enable: exit %d\n%s", code, out)
		}
		if n := rec.count("launchctl bootstrap") + rec.count("enable --now"); n != 2 {
			t.Errorf("start commands = %d, want 2: the running all-addresses gateway answers the loopback probe and must still be restarted: %v", n, rec.all())
		}
		if goos == "linux" && rec.count("systemctl --user restart") != 1 {
			t.Errorf("no restart of the running unit: %v", rec.all())
		}
	})
}

func TestStatusWildcardWithIPv4OnlyListener(t *testing.T) {
	_, dir, state := allAddressesHome(t)
	cfg, _ := remote.LoadConfig(dir)
	host, _ := remote.LoadHost(dir)
	statusLock(t, state)
	ln, err := tls.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)), &tls.Config{Certificates: []tls.Certificate{host.Certificate}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	if st, err := remote.GatewayStatus(state, cfg, host.Fingerprint); err != nil || st != remote.StateRunning {
		t.Fatalf("a healthy listener reachable over IPv4 only reads %q (%v)", st, err)
	}
}

func TestEnablePortChangeRestartsAnsweringUnit(t *testing.T) {
	eachOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos)
		mustEnable(t, freePort(t))
		dir := filepath.Join(home, ".config", "crosstalk")
		host, err := remote.LoadHost(dir)
		if err != nil {
			t.Fatal(err)
		}
		next := freePort(t)
		ln, err := tls.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(next)), &tls.Config{Certificates: []tls.Certificate{host.Certificate}, MinVersion: tls.VersionTLS13})
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { c.(*tls.Conn).Handshake(); c.Close() }()
			}
		}()
		inner := rec.hook
		rec.hook = func(call string) {
			if strings.Contains(call, "restart") || strings.Contains(call, "launchctl bootstrap") {
				ln.Close()
			}
			inner(call)
		}
		if code, out := enableCmd(t, "--port", strconv.Itoa(next), "--yes"); code != 0 {
			t.Fatalf("enable --port: exit %d\n%s", code, out)
		}
		if n := rec.count("launchctl bootstrap") + rec.count("enable --now"); n != 2 {
			t.Errorf("start commands = %d, want 2: a listener already answers on the new port, but the running gateway still holds the old one: %v", n, rec.all())
		}
	})
}
