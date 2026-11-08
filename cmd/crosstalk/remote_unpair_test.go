package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/remote"
)

const fpA, fpB, fpC = "a", "b", "c"

func fp(c string) string { return strings.Repeat(c, 64) }

func unpairEnv(t *testing.T, configure bool) (dir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir = filepath.Join(home, ".config", "crosstalk")
	if configure {
		if code, _, errs := remoteRun(t, false, "config", "--bind", "127.0.0.1", "--alias", "desk", "--yes"); code != 0 {
			t.Fatalf("config: %d %s", code, errs)
		}
	}
	ps := remote.NewPeers(dir)
	for i, c := range []string{fpA, fpB, fpC} {
		err := ps.Upsert(remote.Peer{Alias: []string{"alpha", "beta", "gamma"}[i], Address: "192.0.2.1:7745", Fingerprint: fp(c), PairingIDs: []string{"id-" + c}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func peersJSON(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUnpairKnownAliasRemovesOnlyThatPeer(t *testing.T) {
	dir := unpairEnv(t, true)
	before, _ := remote.NewPeers(dir).List()
	code, out, errs := remoteRun(t, false, "unpair", "beta")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	if want := "unpaired beta; run crosstalk remote unpair desk on beta too\n"; out != want {
		t.Fatalf("stdout %q, want %q", out, want)
	}
	after, err := remote.NewPeers(dir).List()
	if err != nil || len(after) != 2 {
		t.Fatalf("peers after: %v %v", after, err)
	}
	for i, want := range []remote.Peer{before[0], before[2]} {
		got := after[i]
		if got.Alias != want.Alias || got.Fingerprint != want.Fingerprint || got.Address != want.Address ||
			!got.PairedAt.Equal(want.PairedAt) || strings.Join(got.PairingIDs, ",") != strings.Join(want.PairingIDs, ",") {
			t.Errorf("entry %d changed: %+v, want %+v", i, got, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "peers.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("peers.json mode: %v %v", fi, err)
	}
}

func TestUnpairUnknownAliasExits1AndChangesNothing(t *testing.T) {
	dir := unpairEnv(t, true)
	before := peersJSON(t, dir)
	code, out, errs := remoteRun(t, false, "unpair", "nobody")
	if code != 1 || errs == "" || !strings.Contains(errs, "nobody") {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	if strings.Contains(out, "unpaired") || out != "" {
		t.Errorf("stdout on failure: %q", out)
	}
	if after := peersJSON(t, dir); string(after) != string(before) {
		t.Errorf("peers.json changed on an unknown alias")
	}
}

func TestUnpairUnreadablePeersFileIsNotOverwritten(t *testing.T) {
	dir := unpairEnv(t, true)
	p := filepath.Join(dir, "peers.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := remoteRun(t, false, "unpair", "alpha")
	if code != 1 || errs == "" || out != "" || strings.Contains(errs, "no peer") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errs)
	}
	if b := peersJSON(t, dir); string(b) != "{not json" {
		t.Errorf("file rewritten: %q", b)
	}
}

func TestUnpairWithoutRemoteConfigStillRemoves(t *testing.T) {
	dir := unpairEnv(t, false)
	code, out, errs := remoteRun(t, false, "unpair", "alpha")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d stderr %q", code, errs)
	}
	if want := "unpaired alpha; run crosstalk remote unpair <this alias> on alpha too\n"; out != want {
		t.Fatalf("stdout %q, want %q", out, want)
	}
	if ps, _ := remote.NewPeers(dir).List(); len(ps) != 2 {
		t.Fatalf("%d peers left, want 2", len(ps))
	}
}

func TestUnpairBadArgumentsExit2(t *testing.T) {
	dir := unpairEnv(t, true)
	before := peersJSON(t, dir)
	for _, args := range [][]string{{"unpair"}, {"unpair", "alpha", "beta"}, {"unpair", "--nope"}} {
		code, out, errs := remoteRun(t, false, args...)
		if code != 2 || !strings.Contains(errs, "unpair ALIAS") || out != "" {
			t.Errorf("%v: exit %d stdout %q stderr %q", args, code, out, errs)
		}
	}
	if string(peersJSON(t, dir)) != string(before) {
		t.Error("a usage error changed peers.json")
	}
}

func TestRemoteUsageListsUnpair(t *testing.T) {
	_, _, errs := remoteRun(t, false)
	if !strings.Contains(errs, "unpair ALIAS") {
		t.Errorf("usage lacks unpair ALIAS:\n%s", errs)
	}
}

func TestUnpairRefusesKeyOnRunningGatewayWithoutRestart(t *testing.T) {
	dir := unpairEnv(t, true)
	host, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := remote.LoadOrCreateHost(filepath.Join(t.TempDir(), "client"))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.NewPeers(dir).Upsert(remote.Peer{Alias: "c1", Address: "192.0.2.1:7745", Fingerprint: client.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	g := remote.NewGateway(remote.GatewayConfig{
		StateDir: filepath.Join(t.TempDir(), "state"), Host: host, Bind: "127.0.0.1", Port: 0, Peers: remote.NewPeers(dir),
		Limits: remote.DefaultLimits(), Log: func(string, ...any) {},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	})
	finished := make(chan struct{})
	var serveErr error
	ctx, cancel := contextWithCancel()
	go func() { serveErr = g.Serve(ctx); close(finished) }()
	t.Cleanup(func() { cancel(); <-finished })
	select {
	case <-g.Ready():
	case <-finished:
		t.Fatalf("serve: %v", serveErr)
	case <-time.After(5 * time.Second):
		t.Fatal("gateway not ready")
	}
	status := func() (int, error) {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", g.Addr().String(),
			&tls.Config{Certificates: []tls.Certificate{client.Certificate}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
		if err != nil {
			return 0, err
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("GET /x HTTP/1.1\r\nHost: g\r\nConnection: close\r\n\r\n")); err != nil {
			return 0, err
		}
		resp, err := http.ReadResponse(bufioReader(c), nil)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if st, err := status(); err != nil || st != http.StatusNoContent {
		t.Fatalf("before unpair: %d %v", st, err)
	}
	if code, _, errs := remoteRun(t, false, "unpair", "c1"); code != 0 {
		t.Fatalf("unpair: %d %s", code, errs)
	}
	if st, err := status(); err == nil && st != http.StatusForbidden {
		t.Fatalf("after unpair the gateway answered %d, want a refusal", st)
	} else if err == nil && st == http.StatusNoContent {
		t.Fatal("unpaired key still served")
	}
	if st, err := status(); err == nil && st == http.StatusNoContent {
		t.Fatal("unpaired key still served on retry")
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func bufioReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }

func TestUnpairLeadingDashAlias(t *testing.T) {
	dir := unpairEnv(t, true)
	if err := remote.NewPeers(dir).Upsert(remote.Peer{Alias: "-lab", Address: "192.0.2.1:7745", Fingerprint: fp("d")}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := remoteRun(t, false, "unpair", "-lab"); code != 2 {
		t.Errorf("bare -lab: exit %d, want 2", code)
	}
	code, out, errs := remoteRun(t, false, "unpair", "--", "-lab")
	if code != 0 || !strings.HasPrefix(out, "unpaired -lab;") {
		t.Fatalf("unpair -- -lab: %d %q %q", code, out, errs)
	}
	if ps, _ := remote.NewPeers(dir).List(); len(ps) != 3 {
		t.Fatalf("%d peers left, want 3", len(ps))
	}
}
