package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

func remoteRun(t *testing.T, tty bool, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runRemote(args, strings.NewReader(""), tty, &out, &errb)
	return code, out.String(), errb.String()
}

func TestRemoteConfigCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "crosstalk")

	code, out, _ := remoteRun(t, false, "config")
	if code != 0 || !strings.Contains(out, "not configured") {
		t.Fatalf("config before setup: %d %q", code, out)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("reading config created the config directory")
	}

	for _, bad := range [][]string{
		{"config", "--bind", "0.0.0.0", "--yes"},
		{"config", "--bind", "::", "--yes"},
		{"config", "--bind", "not-an-ip", "--yes"},
		{"config", "--bind", "203.0.113.9", "--yes"},
		{"config", "--bind", "127.0.0.1", "--port", "0", "--yes"},
		{"config", "--bind", "127.0.0.1", "--port", "65536", "--yes"},
		{"config", "--bind", "127.0.0.1", "--alias", "a@b", "--yes"},
		{"config", "--port", "7745", "--yes"},
		{"config", "--nope"},
		{"config", "extra"},
		{"config", "--bind", "127.0.0.1"},
		{},
		{"frobnicate"},
	} {
		code, _, errs := remoteRun(t, false, bad...)
		if code != 2 || errs == "" {
			t.Errorf("%v: exit %d stderr %q, want 2 and a message", bad, code, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "remote.json")); err == nil {
		t.Fatal("a refused command wrote remote.json")
	}

	code, out, errs := remoteRun(t, false, "config", "--bind", "127.0.0.1", "--alias", "desk", "--yes")
	if code != 0 {
		t.Fatalf("write: %d %s", code, errs)
	}
	h, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"desk", "127.0.0.1", "7745", h.Fingerprint, filepath.Join(dir, "remote.json")} {
		if !strings.Contains(out, want) {
			t.Errorf("write output lacks %q:\n%s", want, out)
		}
	}
	for f, mode := range map[string]os.FileMode{"remote.json": 0o600, "host.key": 0o600, "host.crt": 0o644} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v mode %v, want %o", f, err, fi, mode)
		}
	}

	code, out2, _ := remoteRun(t, false, "config")
	if code != 0 {
		t.Fatalf("read: %d", code)
	}
	for _, want := range []string{"desk", "127.0.0.1", "7745", h.Fingerprint, dir} {
		if !strings.Contains(out2, want) {
			t.Errorf("read output lacks %q:\n%s", want, out2)
		}
	}

	code, _, errs = remoteRun(t, false, "config", "--bind", "203.0.113.9", "--force", "--yes")
	if code != 0 {
		t.Fatalf("--force with an address off this host: %d %s", code, errs)
	}
	if err := remote.NewPeers(dir).Upsert(remote.Peer{Alias: "lap", Address: "10.0.0.2:7745", Fingerprint: strings.Repeat("e", 64)}); err != nil {
		t.Fatal(err)
	}
	code, out, _ = remoteRun(t, false, "config", "--port", "7800", "--yes")
	if code != 0 || !strings.Contains(out, "peers still dial the old address: pair again with lap") {
		t.Fatalf("changed port: %d\n%s", code, out)
	}
	if h2, _ := remote.LoadHost(dir); h2.Fingerprint != h.Fingerprint {
		t.Error("rewriting the config changed the host key")
	}

	if err := os.WriteFile(filepath.Join(dir, "remote.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := remoteRun(t, false, "config"); code != 1 || !strings.Contains(errs, "remote.json") {
		t.Fatalf("unreadable remote.json: %d %q, want 1 naming the file", code, errs)
	}
}

type hookReader struct{ fn func() }

func (h hookReader) Read(p []byte) (int, error) {
	h.fn()
	return copy(p, "y\n"), nil
}

func TestRemoteConfigOverlappingUpdatesMerge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code, _, errs := remoteRun(t, false, "config", "--bind", "127.0.0.1", "--alias", "desk", "--yes"); code != 0 {
		t.Fatal(errs)
	}
	var out, errb bytes.Buffer
	in := hookReader{func() {
		if code, _, errs := remoteRun(t, false, "config", "--port", "7800", "--yes"); code != 0 {
			t.Error(errs)
		}
	}}
	if code := runRemote([]string{"config", "--alias", "tower"}, in, true, &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	c, err := remote.LoadConfig(filepath.Join(home, ".config", "crosstalk"))
	if err != nil || c.Alias != "tower" || c.Port != 7800 {
		t.Fatalf("config after overlapping updates = %+v, %v; want alias tower and port 7800", c, err)
	}
}

func TestRemoteServeCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code, _, errb := remoteRun(t, false, "serve"); code != 1 || !strings.Contains(errb, "not configured") {
		t.Fatalf("serve with no config: %d %q", code, errb)
	}
	if code, _, _ := remoteRun(t, false, "serve", "extra"); code != 2 {
		t.Fatalf("serve with an argument: %d, want 2", code)
	}
	if code, _, _ := remoteRun(t, false, "bogus"); code != 2 {
		t.Fatalf("unknown command: %d, want 2", code)
	}
}

func TestJoinReadsStdinOnly(t *testing.T) {
	adir := filepath.Join(t.TempDir(), "crosstalk")
	ahost, err := remote.LoadOrCreateHost(adir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	apeers := remote.NewPeers(adir)
	gw := remote.NewGateway(remote.GatewayConfig{StateDir: state, Host: ahost, Bind: "127.0.0.1", Alias: "alpha", Peers: apeers})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-gw.Ready():
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("gateway not ready")
	}
	entry, err := remote.StartPairing(state, time.Now(), 10*time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	tok := remote.PairToken{Address: "127.0.0.1", Port: gw.Addr().(*net.TCPAddr).Port, Fingerprint: ahost.Fingerprint, ID: entry.ID, Secret: entry.Secret}.String()

	home := t.TempDir()
	t.Setenv("HOME", home)
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runRemote(args, strings.NewReader(stdin), false, &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, _, errb := run(tok+"\n", "join"); code != 2 || !strings.Contains(errb, "remote config") {
		t.Fatalf("join with no config: %d %q, want 2 naming remote config", code, errb)
	}
	if code, _, _ := run("", "config", "--bind", "127.0.0.1", "--alias", "beta", "--yes"); code != 0 {
		t.Fatalf("config: %d", code)
	}
	bdir := filepath.Join(home, ".config", "crosstalk")

	code, out, errb := run("", "join", tok)
	if code != 2 {
		t.Fatalf("join with the token as an argument: %d, want 2", code)
	}
	if strings.Contains(out+errb, entry.Secret) {
		t.Fatal("the secret was echoed back")
	}
	if ps, _ := remote.NewPeers(bdir).List(); len(ps) != 0 {
		t.Fatalf("an argument token paired: %+v", ps)
	}
	if ps, _ := apeers.List(); len(ps) != 0 {
		t.Fatalf("an argument token enrolled a peer on the other host: %+v", ps)
	}

	code, out, errb = run(tok+"\nsecond line is never read\n", "join")
	if code != 0 {
		t.Fatalf("join from stdin: %d\nstdout %q\nstderr %q", code, out, errb)
	}
	if !strings.Contains(out, "Paste the token from the other host:") || !strings.Contains(out, "paired with alpha ("+ahost.Fingerprint+")") {
		t.Fatalf("join output: %q", out)
	}
	if strings.Contains(out+errb, entry.Secret) {
		t.Fatal("the secret was echoed back")
	}
	bp, _ := remote.NewPeers(bdir).List()
	if len(bp) != 1 || bp[0].Alias != "alpha" || bp[0].Fingerprint != ahost.Fingerprint || bp[0].Address != gw.Addr().String() {
		t.Fatalf("joiner peers: %+v", bp)
	}
	if ap, _ := apeers.List(); len(ap) != 1 || ap[0].Alias != "beta" {
		t.Fatalf("host A peers: %+v", ap)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRemotePairCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code, _, errb := remoteRun(t, false, "pair"); code != 2 || !strings.Contains(errb, "not configured") {
		t.Fatalf("pair with no config: %d %q, want 2", code, errb)
	}
	if code, _, _ := remoteRun(t, false, "config", "--bind", "127.0.0.1", "--alias", "alpha", "--yes"); code != 0 {
		t.Fatal("config failed")
	}
	if code, _, errb := remoteRun(t, false, "pair"); code != 1 || !strings.Contains(errb, "no gateway running") {
		t.Fatalf("pair with no gateway: %d %q, want 1", code, errb)
	}
	if code, _, _ := remoteRun(t, false, "pair", "extra"); code != 2 {
		t.Fatalf("pair with an argument: %d, want 2", code)
	}

	dir := filepath.Join(home, ".config", "crosstalk")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	host, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	peers := remote.NewPeers(dir)
	gw := remote.NewGateway(remote.GatewayConfig{StateDir: state, Host: host, Bind: "127.0.0.1", Alias: "alpha", Peers: peers})
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
	if tok.Fingerprint != host.Fingerprint || tok.Address != "127.0.0.1" || tok.Port != 7745 {
		t.Fatalf("token %+v", tok)
	}
	tok.Port = gw.Addr().(*net.TCPAddr).Port

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
	case code := <-exit:
		if code != 0 || !strings.Contains(out.String(), "paired with laptop ("+bhost.Fingerprint+")") {
			t.Fatalf("pair: %d out %q err %q", code, out.String(), errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote pair did not notice the redemption")
	}
}

func TestRegisteredA2AURL(t *testing.T) {
	reg := registry.New(t.TempDir())
	publish := func(agentID, url string) {
		t.Helper()
		g, err := reg.Acquire(registry.Request{AgentID: agentID, HarnessName: "n" + agentID, Harness: "claude", Cwd: "/w"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { g.Close() })
		if err := g.Publish(registry.Descriptor{A2AURL: url}); err != nil {
			t.Fatal(err)
		}
	}
	publish("a1a1a1a1a1a1a1a1", "http://127.0.0.1:4001")
	publish("b2b2b2b2b2b2b2b2", "")
	lookup := registeredA2AURL(reg)
	if u, ok := lookup("a1a1a1a1a1a1a1a1"); !ok || u != "http://127.0.0.1:4001" {
		t.Fatalf("registered agent: %q %v", u, ok)
	}
	for name, id := range map[string]string{"registered without a2a_url": "b2b2b2b2b2b2b2b2", "unknown id": "c3c3c3c3c3c3c3c3"} {
		if u, ok := lookup(id); ok {
			t.Fatalf("%s resolved to %q", name, u)
		}
	}
}
