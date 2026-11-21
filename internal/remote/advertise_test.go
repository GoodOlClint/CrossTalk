package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdvertiseDefaultsToBind(t *testing.T) {
	if got := (Config{Bind: "10.0.0.5"}).AdvertiseAddr(); got != "10.0.0.5" {
		t.Fatalf("unset advertise reads %q, want the bind address", got)
	}
	if got := (Config{Bind: "10.0.0.5", Advertise: "box.tail.ts.net"}).AdvertiseAddr(); got != "box.tail.ts.net" {
		t.Fatalf("explicit advertise reads %q", got)
	}
	dir := newDir(t)
	if err := ensureDir(dir); err != nil {
		t.Fatal(err)
	}
	old := `{"version":1,"alias":"desk","bind":"10.0.0.5","port":7745}`
	if err := os.WriteFile(filepath.Join(dir, "remote.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(dir)
	if err != nil || c.AdvertiseAddr() != "10.0.0.5" {
		t.Fatalf("config without advertise: %+v %v", c, err)
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "advertise") {
		t.Fatalf("an unset advertise was written out: %s", b)
	}
}

func TestValidHost(t *testing.T) {
	for _, ok := range []string{"192.168.1.20", "100.64.0.7", "fd7a:115c:a1e0::1", "box", "laptop.local", "host-1.tail1234.ts.net", "box.example.com."} {
		if _, err := ValidHost(ok); err != nil {
			t.Errorf("rejected %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", " ", "*", "*.example.com", "a b", "a b.example.com", "https://box", "box/x", "box?x", "box:7745",
		"0.0.0.0", "::", "224.0.0.1", "fe80::1%en0", "-box", "box-", "a..b", ".box", strings.Repeat("a", 64), "bo_x",
	} {
		if _, err := ValidHost(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestWildcardStillRefusedForBind(t *testing.T) {
	for _, w := range []string{"0.0.0.0", "::", "box.local"} {
		if _, err := ValidateBind(w, true); err == nil {
			t.Errorf("bind accepted %q", w)
		}
	}
}

func TestConfigRejectsBadAdvertise(t *testing.T) {
	for _, bad := range []string{"0.0.0.0", "*", "a b"} {
		c := Config{Version: 1, Alias: "desk", Bind: "10.0.0.5", Advertise: bad, Port: 7745}
		if err := validateConfig(c); err == nil {
			t.Errorf("config with advertise %q accepted", bad)
		}
	}
	c := Config{Version: 1, Alias: "desk", Bind: "10.0.0.5", Advertise: "box.local", Port: 7745}
	if err := validateConfig(c); err != nil {
		t.Fatal(err)
	}
}

func TestPairTokenHostname(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	id, _ := randB64(16)
	sec, _ := randB64(32)
	in := PairToken{Address: "box.tail1234.ts.net", Port: 7745, Fingerprint: fp, ID: id, Secret: sec}
	got, err := ParsePairToken(in.String())
	if err != nil || got != in {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ParsePairToken(strings.Replace(in.String(), in.Address, "*.ts.net", 1)); err == nil {
		t.Error("token with a wildcard name accepted")
	}
}

func TestJoinSendsAdvertiseAddress(t *testing.T) {
	p := startPairGateway(t, nil)
	bdir := newDir(t)
	bhost, _ := LoadOrCreateHost(bdir)
	o := joinOpts(p, bhost, NewPeers(bdir), "beta")
	o.Config.Advertise = "Beta.Tail1234.ts.net"
	if _, err := JoinGateway(context.Background(), o); err != nil {
		t.Fatalf("%v\n%s", err, p.logs)
	}
	ps := p.peerList()
	want := net.JoinHostPort("beta.tail1234.ts.net", strconv.Itoa(o.Config.Port))
	if len(ps) != 1 || ps[0].Address != want {
		t.Fatalf("peer address %+v, want %s (the advertise address, not the bind address)", ps, want)
	}
}

func TestJoinWithoutAdvertiseSendsBind(t *testing.T) {
	p := startPairGateway(t, nil)
	bdir := newDir(t)
	bhost, _ := LoadOrCreateHost(bdir)
	o := joinOpts(p, bhost, NewPeers(bdir), "beta")
	if _, err := JoinGateway(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if ps := p.peerList(); len(ps) != 1 || ps[0].Address != "127.0.0.1:7746" {
		t.Fatalf("%+v", ps)
	}
}

func TestGatewayRejectsWildcardPeerAddress(t *testing.T) {
	if _, err := validPeerAddress("0.0.0.0:7745"); err == nil {
		t.Error("wildcard peer address accepted")
	}
	if a, err := validPeerAddress("Box.Local:7745"); err != nil || a != "box.local:7745" {
		t.Errorf("%q %v", a, err)
	}
}

func TestPeerByNameResolvedAtConnectTime(t *testing.T) {
	e := agentsGateway(t, nil)
	pinned := e.pin("desk1")
	_, port, _ := net.SplitHostPort(e.addr())
	var lookups atomic.Int32
	answer := atomic.Value{}
	answer.Store("::2")
	old := LookupHost
	LookupHost = func(_ context.Context, h string) ([]string, error) {
		lookups.Add(1)
		if h != "peer.tail1234.ts.net" {
			return nil, errors.New("unexpected name " + h)
		}
		return []string{answer.Load().(string)}, nil
	}
	t.Cleanup(func() { LookupHost = old })

	peer := Peer{Alias: "gw", Address: net.JoinHostPort("peer.tail1234.ts.net", port), Fingerprint: e.host.Fingerprint}
	c := NewClient(pinned, peer)
	c.Timeout = 2 * time.Second
	if _, err := c.Agents(context.Background()); err == nil {
		t.Fatal("reached the gateway at an address it does not listen on")
	}
	answer.Store("127.0.0.1")
	if rows, err := c.Agents(context.Background()); err != nil || len(rows) == 0 {
		t.Fatalf("after the name moved to the gateway's address: %v %v", rows, err)
	}
	if lookups.Load() < 2 {
		t.Fatalf("name resolved %d times, want once per connection", lookups.Load())
	}
}

func TestPinStillRejectsWrongCertAtRightName(t *testing.T) {
	e := agentsGateway(t, nil)
	pinned := e.pin("desk1")
	_, port, _ := net.SplitHostPort(e.addr())
	old := LookupHost
	LookupHost = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	t.Cleanup(func() { LookupHost = old })

	peer := Peer{Alias: "gw", Address: net.JoinHostPort("peer.local", port), Fingerprint: strings.Repeat("00", 32)}
	_, err := NewClient(pinned, peer).Agents(context.Background())
	var re *Error
	if !errors.As(err, &re) || re.Reason != ReasonPinMismatch {
		t.Fatalf("wrong certificate at a resolvable name: %v, want %s", err, ReasonPinMismatch)
	}
}

func TestValidHostKeepsFinalDot(t *testing.T) {
	if h, err := ValidHost("Box.Example.com."); err != nil || h != "box.example.com." {
		t.Fatalf("%q %v", h, err)
	}
	if _, err := ValidHost("."); err == nil {
		t.Error("accepted .")
	}
}

func TestDialPeerSkipsStalledAddress(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	old := LookupHost
	LookupHost = func(context.Context, string) ([]string, error) { return []string{"192.0.2.1", "127.0.0.1"}, nil }
	t.Cleanup(func() { LookupHost = old })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	c, err := DialPeer(ctx, "tcp", net.JoinHostPort("peer.local", port))
	if err != nil {
		t.Fatalf("a stalled first address hid the reachable second: %v", err)
	}
	c.Close()
}
