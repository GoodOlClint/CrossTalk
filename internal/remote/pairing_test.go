package remote

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type pairEnv struct {
	*gwEnv
	clk   *fakeClock
	entry Pairing
}

func startPairGateway(t *testing.T, mut func(*GatewayConfig)) *pairEnv {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	e := startGateway(t, func(c *GatewayConfig) {
		c.Now = clk.Now
		c.Alias = "alpha"
		c.Limits.PendingPoll = time.Hour
		if mut != nil {
			mut(c)
		}
	})
	p := &pairEnv{gwEnv: e, clk: clk}
	entry, err := StartPairing(e.stateDir, clk.Now(), 10*time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	p.entry = entry
	return p
}

func pairMAC(secret, clientFP, serverFP string) string {
	key, _ := base64.RawURLEncoding.DecodeString(secret)
	h := hmac.New(sha256.New, key)
	h.Write([]byte("crosstalk-pair-v1" + clientFP + serverFP))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func randSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (p *pairEnv) good(clientFP, alias string) pairRequest {
	return pairRequest{ID: p.entry.ID, Alias: alias, Address: "127.0.0.1:7746", MAC: pairMAC(p.entry.Secret, clientFP, p.host.Fingerprint)}
}

func (p *pairEnv) post(c *http.Client, body any) (int, string) {
	p.t.Helper()
	var b []byte
	switch v := body.(type) {
	case string:
		b = []byte(v)
	default:
		b, _ = json.Marshal(v)
	}
	resp, err := c.Post("https://"+p.addr()+"/v1/pair", "application/json", bytes.NewReader(b))
	if err != nil {
		p.t.Fatalf("POST /v1/pair: %v\nlogs:\n%s", err, p.logs)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (p *pairEnv) get(c *http.Client, method, path string) int {
	p.t.Helper()
	req, _ := http.NewRequest(method, "https://"+p.addr()+path, strings.NewReader("{}"))
	resp, err := c.Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// open returns an unenrolled client whose TLS connection is already up.
func (p *pairEnv) open() (Host, *http.Client) {
	p.t.Helper()
	h := p.newClient(false)
	c := p.httpClient(h)
	if st := p.get(c, "GET", "/warm"); st != http.StatusForbidden {
		p.t.Fatalf("warm-up request: %d, want 403", st)
	}
	return h, c
}

func (p *pairEnv) files() (pending, peers []byte) {
	pending, _ = os.ReadFile(filepath.Join(p.stateDir, "remote", "pending.json"))
	peers, _ = os.ReadFile(filepath.Join(p.cfgDir, "peers.json"))
	return
}

func (p *pairEnv) mustBeUnchanged(pending, peers []byte, what string) {
	p.t.Helper()
	np, nq := p.files()
	if !bytes.Equal(pending, np) {
		p.t.Fatalf("%s changed pending.json:\nbefore %s\nafter  %s", what, pending, np)
	}
	if !bytes.Equal(peers, nq) {
		p.t.Fatalf("%s changed peers.json:\nbefore %s\nafter  %s", what, peers, nq)
	}
}

func (p *pairEnv) step() { p.clk.Advance(1100 * time.Millisecond) }

func (p *pairEnv) peerList() []Peer {
	ps, err := p.peers.List()
	if err != nil {
		p.t.Fatal(err)
	}
	return ps
}

func TestPairWrongSecret(t *testing.T) {
	p := startPairGateway(t, nil)
	x, c := p.open()
	pending, peers := p.files()
	if len(pending) == 0 {
		t.Fatal("StartPairing wrote no pending.json")
	}

	bad := p.good(x.Fingerprint, "beta")
	bad.MAC = pairMAC(randSecret(), x.Fingerprint, p.host.Fingerprint)
	if st, _ := p.post(c, bad); st != http.StatusForbidden {
		t.Fatalf("wrong secret: %d, want 403", st)
	}
	p.mustBeUnchanged(pending, peers, "a wrong secret")

	p.step()
	unknown := p.good(x.Fingerprint, "beta")
	unknown.ID = "AAAAAAAAAAAAAAAAAAAAAA"
	if st, _ := p.post(c, unknown); st != http.StatusNotFound {
		t.Fatalf("unknown id: %d, want 404", st)
	}
	p.mustBeUnchanged(pending, peers, "an unknown id")

	p.step()
	if st, _ := p.post(c, "{not json"); st != http.StatusBadRequest {
		t.Fatalf("malformed body: %d, want 400", st)
	}
	p.mustBeUnchanged(pending, peers, "a malformed body")

	p.step()
	st, body := p.post(c, p.good(x.Fingerprint, "beta"))
	if st != http.StatusOK || !strings.Contains(body, `"alpha"`) {
		t.Fatalf("real proof after failures: %d %s, want 200 announcing alpha", st, body)
	}
	ps := p.peerList()
	if len(ps) != 1 || ps[0].Fingerprint != x.Fingerprint || ps[0].Alias != "beta" || ps[0].Address != "127.0.0.1:7746" {
		t.Fatalf("peers after pairing: %+v", ps)
	}
}

func TestPairExpired(t *testing.T) {
	p := startPairGateway(t, nil)
	x, c := p.open()
	pending, peers := p.files()
	p.clk.Advance(10 * time.Minute)
	if st, _ := p.post(c, p.good(x.Fingerprint, "beta")); st != http.StatusGone {
		t.Fatalf("redeem at the expiry instant: %d, want 410", st)
	}
	p.mustBeUnchanged(pending, peers, "an expired attempt")

	q := startPairGateway(t, nil)
	y, d := q.open()
	q.clk.Advance(10*time.Minute - time.Nanosecond)
	if st, body := q.post(d, q.good(y.Fingerprint, "beta")); st != http.StatusOK {
		t.Fatalf("redeem one nanosecond before expiry: %d %s, want 200", st, body)
	}
}

func TestPairSingleUse(t *testing.T) {
	p := startPairGateway(t, nil)
	x, cx := p.open()
	y, cy := p.open()
	if st, body := p.post(cx, p.good(x.Fingerprint, "xray")); st != http.StatusOK {
		t.Fatalf("first redemption: %d %s", st, body)
	}
	pending, peers := p.files()
	p.step()
	if st, _ := p.post(cy, p.good(y.Fingerprint, "yankee")); st != http.StatusNotFound {
		t.Fatalf("second redemption of one token by another client: %d, want 404", st)
	}
	p.mustBeUnchanged(pending, peers, "a second redemption")
	if ps := p.peerList(); len(ps) != 1 || ps[0].Fingerprint != x.Fingerprint {
		t.Fatalf("peers after second redemption: %+v", ps)
	}

	p.step()
	hits := p.hits.Load()
	st, _ := p.post(cx, p.good(x.Fingerprint, "xray"))
	if st == http.StatusOK && p.hits.Load() == hits {
		t.Fatal("the winner redeemed the same token again")
	}
	p.mustBeUnchanged(pending, peers, "a second redemption by the winner")
}

func TestPairConcurrentRedeem(t *testing.T) {
	const n = 8
	p := startPairGateway(t, func(c *GatewayConfig) {
		c.Limits.MaxUnenrolled = 16
		c.Limits.PairInterval = -1
	})
	hosts := make([]Host, n)
	clients := make([]*http.Client, n)
	for i := range hosts {
		hosts[i], clients[i] = p.open()
	}
	codes := make([]int, n)
	var ready, wg sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			body, _ := json.Marshal(p.good(hosts[i].Fingerprint, fmt.Sprintf("peer%d", i)))
			resp, err := clients[i].Post("https://"+p.addr()+"/v1/pair", "application/json", bytes.NewReader(body))
			if err != nil {
				codes[i] = -1
				return
			}
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	winner := -1
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			if winner >= 0 {
				t.Fatalf("two winners: %d and %d (codes %v)", winner, i, codes)
			}
			winner = i
		case http.StatusNotFound:
		default:
			t.Fatalf("client %d got %d (codes %v)", i, c, codes)
		}
	}
	if winner < 0 {
		t.Fatalf("no winner (codes %v)", codes)
	}
	ps := p.peerList()
	if len(ps) != 1 || ps[0].Fingerprint != hosts[winner].Fingerprint {
		t.Fatalf("peers %+v, want only client %d", ps, winner)
	}
	if pending, _ := p.files(); bytes.Contains(pending, []byte(p.entry.ID)) {
		t.Fatalf("the redeemed entry is still in pending.json: %s", pending)
	}
	left, _ := filepath.Glob(filepath.Join(p.cfgDir, "*.tmp*"))
	more, _ := filepath.Glob(filepath.Join(p.stateDir, "remote", "*.tmp*"))
	if len(left)+len(more) != 0 {
		t.Fatalf("temp files left behind: %v %v", left, more)
	}
}

const redeemChildEnv = "CROSSTALK_TEST_REDEEM_CHILD"

func TestRedeemChild(t *testing.T) {
	if os.Getenv(redeemChildEnv) == "" {
		t.Skip("helper process for TestPairConcurrentRedeemAcrossProcesses")
	}
	state, cfg := os.Getenv("RC_STATE"), os.Getenv("RC_CFG")
	fp := os.Getenv("RC_FP")
	for {
		if _, err := os.Stat(os.Getenv("RC_GO")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	req := pairRequest{ID: os.Getenv("RC_ID"), Alias: "peer-" + fp[:6], Address: "127.0.0.1:7746", MAC: pairMAC(os.Getenv("RC_SECRET"), fp, os.Getenv("RC_SERVER"))}
	if err := redeemPairing(state, NewPeers(cfg), time.Now(), req, fp, os.Getenv("RC_SERVER")); err != nil {
		fmt.Println("refused", err)
		return
	}
	fmt.Println("enrolled", fp)
}

func TestPairConcurrentRedeemAcrossProcesses(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	cfg := newDir(t)
	if err := ensureDir(state); err != nil {
		t.Fatal(err)
	}
	if err := ensureDir(cfg); err != nil {
		t.Fatal(err)
	}
	entry, err := StartPairing(state, time.Now(), 10*time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	server := strings.Repeat("ab", 32)
	goFile := filepath.Join(t.TempDir(), "go")
	const n = 4
	outs := make([]*bytes.Buffer, n)
	cmds := make([]*exec.Cmd, n)
	fps := make([]string, n)
	for i := range cmds {
		fps[i] = fmt.Sprintf("%064x", i+1)
		outs[i] = &bytes.Buffer{}
		cmd := exec.Command(os.Args[0], "-test.run=^TestRedeemChild$", "-test.v")
		cmd.Env = append(os.Environ(), redeemChildEnv+"=1", "RC_STATE="+state, "RC_CFG="+cfg, "RC_FP="+fps[i],
			"RC_ID="+entry.ID, "RC_SECRET="+entry.Secret, "RC_SERVER="+server, "RC_GO="+goFile)
		cmd.Stdout = outs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	time.Sleep(300 * time.Millisecond)
	os.WriteFile(goFile, nil, 0o600)
	winners := 0
	winner := ""
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, outs[i])
		}
		if strings.Contains(outs[i].String(), "enrolled "+fps[i]) {
			winners++
			winner = fps[i]
		}
	}
	if winners != 1 {
		t.Fatalf("%d children enrolled, want exactly 1", winners)
	}
	ps, err := NewPeers(cfg).List()
	if err != nil || len(ps) != 1 || ps[0].Fingerprint != winner {
		t.Fatalf("peers %+v %v, want only %s", ps, err, winner)
	}
}

func TestPairBindsClientKey(t *testing.T) {
	p := startPairGateway(t, nil)
	x, cx := p.open()
	y := p.newClient(false)
	pending, peers := p.files()

	forY := p.good(y.Fingerprint, "beta")
	if st, _ := p.post(cx, forY); st != http.StatusForbidden {
		t.Fatalf("a MAC made for another client's fingerprint: %d, want 403", st)
	}
	p.mustBeUnchanged(pending, peers, "a MAC for another fingerprint")

	p.step()
	wrongServer := p.good(x.Fingerprint, "beta")
	wrongServer.MAC = pairMAC(p.entry.Secret, x.Fingerprint, x.Fingerprint)
	if st, _ := p.post(cx, wrongServer); st != http.StatusForbidden {
		t.Fatalf("a MAC made for another server fingerprint: %d, want 403", st)
	}
	p.mustBeUnchanged(pending, peers, "a MAC for another server")

	p.step()
	body := map[string]string{"id": p.entry.ID, "alias": "beta", "address": "127.0.0.1:7746", "fingerprint": y.Fingerprint,
		"mac": pairMAC(p.entry.Secret, x.Fingerprint, p.host.Fingerprint)}
	if st, out := p.post(cx, body); st != http.StatusOK {
		t.Fatalf("right MAC: %d %s", st, out)
	}
	if ps := p.peerList(); len(ps) != 1 || ps[0].Fingerprint != x.Fingerprint {
		t.Fatalf("peers %+v, want the handshake fingerprint %s and not the body's", ps, x.Fingerprint)
	}
}

func TestUnenrolledOnlyPair(t *testing.T) {
	p := startPairGateway(t, nil)
	x, c := p.open()
	for _, probe := range [][2]string{{"GET", "/v1/agents"}, {"POST", "/v1/agents"}, {"GET", "/v1/pair"}, {"POST", "/v1/a2a/0123456789abcdef"}, {"GET", "/"}} {
		if st := p.get(c, probe[0], probe[1]); st != http.StatusForbidden {
			t.Fatalf("unenrolled %s %s: %d, want 403", probe[0], probe[1], st)
		}
	}
	if p.hits.Load() != 0 {
		t.Fatal("the handler ran for an unenrolled connection")
	}
	if st, body := p.post(c, p.good(x.Fingerprint, "beta")); st != http.StatusOK {
		t.Fatalf("POST /v1/pair on an unenrolled connection: %d %s, want 200", st, body)
	}

	q := startPairGateway(t, func(c *GatewayConfig) { c.Handler = nil })
	pinned := q.newClient(true)
	pc := q.httpClient(pinned)
	pending, _ := q.files()
	if st, _ := q.post(pc, q.good(pinned.Fingerprint, "beta")); st != http.StatusNotFound {
		t.Fatalf("a pinned peer on /v1/pair: %d, want the existing 404", st)
	}
	np, _ := q.files()
	if !bytes.Equal(pending, np) {
		t.Fatal("a pinned peer's /v1/pair changed pending.json")
	}
}

func TestUnenrolledPoolBounded(t *testing.T) {
	p := startPairGateway(t, func(c *GatewayConfig) { c.Limits.UnenrolledLife = 1500 * time.Millisecond })
	for i := 0; i < 4; i++ {
		c, err := p.dial(clientTLS(p.newClient(false), 0))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		io.WriteString(c, "POST /v1/pair HTTP/1.1\r\nHost: g\r\nContent-Length: 100\r\n\r\n")
	}
	time.Sleep(100 * time.Millisecond)
	if c, err := p.dial(clientTLS(p.newClient(false), 0)); err == nil {
		defer c.Close()
		if st, err := roundTrip(c); err == nil {
			t.Fatalf("a fifth unenrolled connection was served (%d) while 4 stalled", st)
		}
	}
	pinned, err := p.dial(clientTLS(p.newClient(true), 0))
	if err != nil {
		t.Fatalf("a pinned peer cannot connect while the unenrolled pool is full: %v", err)
	}
	defer pinned.Close()
	if st, err := roundTrip(pinned); err != nil || st != http.StatusNoContent {
		t.Fatalf("pinned peer: %d %v", st, err)
	}

	z := p.newClient(false)
	cz := p.httpClient(z)
	deadline := time.Now().Add(6 * time.Second)
	for {
		resp, err := cz.Post("https://"+p.addr()+"/v1/pair", "application/json", bytes.NewReader(mustJSON(p.good(z.Fingerprint, "zulu"))))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pairing after the stalled connections were closed (%v)", err)
		}
		p.step()
		time.Sleep(50 * time.Millisecond)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestUnenrolledRefusedWithoutPending(t *testing.T) {
	e := startGateway(t, func(c *GatewayConfig) {
		c.Alias = "alpha"
		c.Limits.PendingPoll = time.Hour
	})
	refused := func(what string) {
		t.Helper()
		x := e.newClient(false)
		c, err := e.dial(clientTLS(x, 0))
		if err != nil {
			return
		}
		defer c.Close()
		if st, err := roundTrip(c); err == nil {
			t.Fatalf("%s: an unenrolled client got HTTP %d", what, st)
		}
	}
	refused("no pairing ever started")

	entry, err := StartPairing(e.stateDir, time.Now(), 10*time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.dial(clientTLS(e.newClient(false), 0))
	if err != nil {
		t.Fatalf("with a pairing pending: %v", err)
	}
	if st, err := roundTrip(c); err != nil || st != http.StatusForbidden {
		t.Fatalf("with a pairing pending: %d %v, want 403", st, err)
	}
	c.Close()
	if err := CancelPairing(e.stateDir, entry.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	refused("after the pairing was cancelled")
	if _, err := StartPairing(e.stateDir, time.Now().Add(-time.Hour), 10*time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	refused("with only an expired pairing in the file")
}

func TestUnenrolledClosedOnExpiry(t *testing.T) {
	p := startPairGateway(t, func(c *GatewayConfig) { c.Limits.PendingPoll = 20 * time.Millisecond })
	c, err := p.dial(clientTLS(p.newClient(false), 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if st, err := roundTrip(c); err != nil || st != http.StatusForbidden {
		t.Fatalf("before expiry: %d %v", st, err)
	}
	p.clk.Advance(10 * time.Minute)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the unenrolled connection was still open 3 s after the pairing expired (%v)", err)
	}
}

func TestWrongProofsCannotCancelPairing(t *testing.T) {
	p := startPairGateway(t, nil)
	_, c := p.open()
	x := p.newClient(false)
	pending, peers := p.files()
	for i := 0; i < 30; i++ {
		p.step()
		req := p.good(x.Fingerprint, "beta")
		switch i % 3 {
		case 0:
			req.MAC = pairMAC(randSecret(), x.Fingerprint, p.host.Fingerprint)
		case 1:
			req.ID = fmt.Sprintf("unknown-%02d", i)
		case 2:
			req.MAC = "!!"
		}
		if st, _ := p.post(c, req); st < 400 {
			t.Fatalf("attempt %d: %d, want a refusal", i, st)
		}
		p.mustBeUnchanged(pending, peers, fmt.Sprintf("attempt %d", i))
	}
	p.step()

	bdir := newDir(t)
	bhost, err := LoadOrCreateHost(bdir)
	if err != nil {
		t.Fatal(err)
	}
	bpeers := NewPeers(bdir)
	got, err := JoinGateway(context.Background(), JoinOptions{
		Host: bhost, Config: Config{Version: 1, Alias: "beta", Bind: "127.0.0.1", Port: 7746}, Peers: bpeers,
		Token:   PairToken{Address: "127.0.0.1", Port: p.gw.Addr().(*net.TCPAddr).Port, Fingerprint: p.host.Fingerprint, ID: p.entry.ID, Secret: p.entry.Secret},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("the real join after wrong proofs: %v\nlogs:\n%s", err, p.logs)
	}
	if got.Alias != "alpha" || got.Fingerprint != p.host.Fingerprint {
		t.Fatalf("joiner recorded %+v", got)
	}
	ps := p.peerList()
	if len(ps) != 1 || ps[0].Fingerprint != bhost.Fingerprint || ps[0].Alias != "beta" || ps[0].Address != "127.0.0.1:7746" {
		t.Fatalf("host A peers: %+v", ps)
	}
	if bl, _ := bpeers.List(); len(bl) != 1 || bl[0].Alias != "alpha" {
		t.Fatalf("host B peers: %+v", bl)
	}
}

func TestPairRateLimited(t *testing.T) {
	p := startPairGateway(t, nil)
	x, c := p.open()
	pending, peers := p.files()

	wrong := p.good(x.Fingerprint, "beta")
	wrong.MAC = pairMAC(randSecret(), x.Fingerprint, p.host.Fingerprint)
	if st, _ := p.post(c, wrong); st != http.StatusForbidden {
		t.Fatalf("first attempt: %d, want 403", st)
	}
	if st, _ := p.post(c, p.good(x.Fingerprint, "beta")); st != http.StatusTooManyRequests {
		t.Fatalf("second attempt in the same second, with a good proof: %d, want 429", st)
	}
	p.mustBeUnchanged(pending, peers, "a rate-limited attempt")

	p.clk.Advance(999 * time.Millisecond)
	if st, _ := p.post(c, p.good(x.Fingerprint, "beta")); st != http.StatusTooManyRequests {
		t.Fatalf("attempt 999 ms later: %d, want 429", st)
	}
	p.clk.Advance(time.Millisecond)
	if st, body := p.post(c, p.good(x.Fingerprint, "beta")); st != http.StatusOK {
		t.Fatalf("attempt one second after the first: %d %s, want 200", st, body)
	}
}
