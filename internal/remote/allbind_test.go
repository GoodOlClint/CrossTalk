package remote

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllAddressesGatewayListensOnWildcard(t *testing.T) {
	e := newGateway(t, func(c *GatewayConfig) { c.Bind = AllBind }).start()
	ta := e.gw.Addr().(*net.TCPAddr)
	if !ta.IP.IsUnspecified() {
		t.Fatalf("listening on %s, want the wildcard", ta)
	}
	client := e.newClient(true)
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ta.Port)), clientTLS(client, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if code, err := roundTrip(c); err != nil || code != 204 {
		t.Fatalf("round trip over loopback: %d, %v", code, err)
	}
}

func TestWildcardGatewayNeverWaitsForAddress(t *testing.T) {
	var lists atomic.Int32
	e := newGateway(t, func(c *GatewayConfig) {
		c.Bind = AllBind
		c.Limits.AddrPoll = 5 * time.Millisecond
		c.Addrs = func() ([]net.Addr, error) { lists.Add(1); return nil, nil }
	}).start()
	time.Sleep(60 * time.Millisecond)
	if s := e.gw.State(); s != stateListening {
		t.Fatalf("state %q with no interface addresses, want listening", s)
	}
	if n := lists.Load(); n != 0 {
		t.Fatalf("a wildcard gateway listed interface addresses %d times", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !e.gw.waitAddrBack(ctx) {
		t.Fatal("waitAddrBack blocked on a wildcard bind")
	}
}

func TestWildcardBindNeverBecomesAdvertise(t *testing.T) {
	for _, b := range []string{"0.0.0.0", "::"} {
		if got := (Config{Bind: b}).AdvertiseAddr(); got != "" {
			t.Errorf("bind %s, no advertise: AdvertiseAddr = %q", b, got)
		}
		if got := (Config{Bind: b, Advertise: "box.ts.net"}).AdvertiseAddr(); got != "box.ts.net" {
			t.Errorf("bind %s: AdvertiseAddr = %q", b, got)
		}
	}
}

func TestLoadConfigWildcardNeedsAdvertise(t *testing.T) {
	for _, b := range []string{"0.0.0.0", "::"} {
		dir := newDir(t)
		if err := ensureDir(dir); err != nil {
			t.Fatal(err)
		}
		write := func(body string) {
			if err := os.WriteFile(filepath.Join(dir, "remote.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		write(`{"version":1,"alias":"desk","bind":"` + b + `","port":7745}`)
		if _, err := LoadConfig(dir); err == nil {
			t.Errorf("bind %s without advertise loaded", b)
		}
		for _, bad := range []string{"0.0.0.0", "::"} {
			write(`{"version":1,"alias":"desk","bind":"` + b + `","advertise":"` + bad + `","port":7745}`)
			if _, err := LoadConfig(dir); err == nil {
				t.Errorf("bind %s with wildcard advertise %s loaded", b, bad)
			}
		}
		write(`{"version":1,"alias":"desk","bind":"` + b + `","advertise":"box.ts.net","port":7745}`)
		c, err := LoadConfig(dir)
		if b == AllBind && (err != nil || c.AdvertiseAddr() != "box.ts.net") {
			t.Errorf("bind %s with advertise: %+v %v", b, c, err)
		}
		if b != AllBind && err == nil {
			t.Errorf("non-canonical wildcard %s loaded: it would listen dual-stack", b)
		}
	}
	dir := newDir(t)
	if err := ensureDir(dir); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"alias":"desk","bind":"::ffff:0.0.0.0","advertise":"box.ts.net","port":7745}`
	if err := os.WriteFile(filepath.Join(dir, "remote.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Error("a mapped wildcard bind loaded")
	}
}

func TestWildcardStillRefusedOutsideMode(t *testing.T) {
	for _, w := range []string{"0.0.0.0", "::", "[::]", "::ffff:0.0.0.0"} {
		if _, err := ValidateBind(w, true); err == nil {
			t.Errorf("ValidateBind accepted %q", w)
		}
		if _, err := ValidHost(w); err == nil {
			t.Errorf("ValidHost accepted %q", w)
		}
	}
	if err := validateConfig(Config{Version: 1, Alias: "desk", Bind: "224.0.0.1", Advertise: "box.ts.net", Port: 7745}); err == nil {
		t.Error("multicast bind accepted")
	}
	if _, err := ParsePairToken(PairToken{Address: "::", Port: 7745, Fingerprint: "ab", ID: "x", Secret: "y"}.String()); err == nil {
		t.Error("wildcard token address accepted")
	}
	if _, err := validPeerAddress("[::]:7745"); err == nil {
		t.Error("wildcard peer address accepted")
	}
}

func TestJoinFromAllAddressesSendsAdvertise(t *testing.T) {
	p := startPairGateway(t, nil)
	bdir := newDir(t)
	bhost, _ := LoadOrCreateHost(bdir)
	o := joinOpts(p, bhost, NewPeers(bdir), "beta")
	o.Config.Bind, o.Config.Advertise = AllBind, "beta.ts.net"
	if _, err := JoinGateway(context.Background(), o); err != nil {
		t.Fatalf("%v\n%s", err, p.logs)
	}
	ps := p.peerList()
	want := net.JoinHostPort("beta.ts.net", strconv.Itoa(o.Config.Port))
	if len(ps) != 1 || ps[0].Address != want {
		t.Fatalf("peer address %+v, want %s", ps, want)
	}
}

func TestDialBindsAndBindOnHost(t *testing.T) {
	if got := DialBinds("::"); len(got) != 2 || got[0] != "127.0.0.1" || got[1] != "::1" {
		t.Errorf("DialBinds(::) = %v, want both loopbacks", got)
	}
	if got := DialBinds("10.0.0.5"); len(got) != 1 || got[0] != "10.0.0.5" {
		t.Errorf("DialBinds(10.0.0.5) = %v", got)
	}
	old := InterfaceAddrs
	InterfaceAddrs = func() ([]net.Addr, error) { return nil, nil }
	defer func() { InterfaceAddrs = old }()
	if !BindOnHost("::") || !BindOnHost("0.0.0.0") {
		t.Error("a wildcard bind reads as not on this host")
	}
	if BindOnHost("10.0.0.5") {
		t.Error("an absent address reads as on this host")
	}
}

func TestTailnetAddr(t *testing.T) {
	old := InterfaceAddrs
	defer func() { InterfaceAddrs = old }()
	set := func(ip string, bits int) {
		InterfaceAddrs = func() ([]net.Addr, error) {
			p, n := net.ParseIP(ip), 128
			if p4 := p.To4(); p4 != nil {
				p, n = p4, 32
			}
			return []net.Addr{&net.IPNet{IP: p, Mask: net.CIDRMask(bits, n)}}, nil
		}
	}
	set("100.101.102.103", 32)
	if a, ok := TailnetAddr(); !ok || a != "100.101.102.103" {
		t.Errorf("v4: %q %v", a, ok)
	}
	set("fd7a:115c:a1e0::1", 128)
	if a, ok := TailnetAddr(); !ok || a != "fd7a:115c:a1e0::1" {
		t.Errorf("v6: %q %v", a, ok)
	}
	set("192.168.1.5", 24)
	if _, ok := TailnetAddr(); ok {
		t.Error("a LAN address read as a tailnet address")
	}
}
