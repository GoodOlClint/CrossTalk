package pairing

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const serverFP = "server-fingerprint-for-the-spike"

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	s := &Store{Dir: t.TempDir(), ServerFP: serverFP, Now: func() time.Time { return t0 }}
	id, secret, err := s.Start(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return s, id, secret
}

type snap struct {
	exists bool
	data   []byte
	mtime  time.Time
}

func snapshot(t *testing.T, s *Store) map[string]snap {
	t.Helper()
	o := map[string]snap{}
	for _, n := range []string{"pending.json", "peers.json"} {
		fi, err := os.Stat(s.path(n))
		if errors.Is(err, os.ErrNotExist) {
			o[n] = snap{}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(s.path(n))
		o[n] = snap{true, b, fi.ModTime()}
	}
	return o
}

func same(t *testing.T, a, b map[string]snap) {
	t.Helper()
	for n := range a {
		if a[n].exists != b[n].exists || !bytes.Equal(a[n].data, b[n].data) || !a[n].mtime.Equal(b[n].mtime) {
			t.Fatalf("%s changed on a failed redemption", n)
		}
	}
}

func fpOf(n string) string { h := sha256.Sum256([]byte(n)); return hex.EncodeToString(h[:]) }

func TestP1_WrongMACKnownID(t *testing.T) {
	s, id, secret := newStore(t)
	before := snapshot(t, s)
	if err := s.Redeem(id, MAC([]byte("not the secret"), fpOf("x"), serverFP), fpOf("x"), "x", "127.0.0.1:1"); !errors.Is(err, ErrBadMAC) {
		t.Fatalf("%v", err)
	}
	same(t, before, snapshot(t, s))
	if err := s.Redeem(id, MAC(secret, fpOf("x"), serverFP), fpOf("x"), "x", "127.0.0.1:1"); err != nil {
		t.Fatalf("the real redemption after a wrong proof: %v", err)
	}
}

func TestP1_UnknownID(t *testing.T) {
	s, _, secret := newStore(t)
	before := snapshot(t, s)
	if err := s.Redeem("0000000000000000", MAC(secret, fpOf("x"), serverFP), fpOf("x"), "x", "a"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("%v", err)
	}
	same(t, before, snapshot(t, s))
}

func TestP1_MACBoundToOtherFingerprint(t *testing.T) {
	s, id, secret := newStore(t)
	before := snapshot(t, s)
	if err := s.Redeem(id, MAC(secret, fpOf("x"), serverFP), fpOf("y"), "y", "a"); !errors.Is(err, ErrBadMAC) {
		t.Fatalf("%v", err)
	}
	same(t, before, snapshot(t, s))
}

func TestP2_ExpiredToken(t *testing.T) {
	s, id, secret := newStore(t)
	exp := t0.Add(10 * time.Minute)
	s.Now = func() time.Time { return exp.Add(time.Nanosecond) }
	before := snapshot(t, s)
	if err := s.Redeem(id, MAC(secret, fpOf("x"), serverFP), fpOf("x"), "x", "a"); !errors.Is(err, ErrExpired) {
		t.Fatalf("%v", err)
	}
	same(t, before, snapshot(t, s))
	s.Now = func() time.Time { return exp.Add(-time.Nanosecond) }
	if err := s.Redeem(id, MAC(secret, fpOf("x"), serverFP), fpOf("x"), "x", "a"); err != nil {
		t.Fatalf("control, one ns before expiry: %v", err)
	}
}

func TestP3_SecondRedemption(t *testing.T) {
	s, id, secret := newStore(t)
	if err := s.Redeem(id, MAC(secret, fpOf("x"), serverFP), fpOf("x"), "x", "a"); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, s)
	for _, who := range []string{"x", "y"} {
		if err := s.Redeem(id, MAC(secret, fpOf(who), serverFP), fpOf(who), who, "a"); !errors.Is(err, ErrUnknown) {
			t.Fatalf("second redemption by %s: %v", who, err)
		}
	}
	same(t, before, snapshot(t, s))
	if p := s.Peers(); len(p) != 1 || p[0].FP != fpOf("x") {
		t.Fatalf("%+v", p)
	}
}

func concurrent(t *testing.T, n int) {
	s, id, secret := newStore(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	Writes.Store(0)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fp := fpOf(fmt.Sprint("client", i))
			<-start
			errs[i] = s.Redeem(id, MAC(secret, fp, serverFP), fp, fmt.Sprint("c", i), "a")
		}()
	}
	close(start)
	wg.Wait()
	win := -1
	for i, e := range errs {
		switch {
		case e == nil && win < 0:
			win = i
		case e == nil:
			t.Fatalf("two winners: %d and %d", win, i)
		case !errors.Is(e, ErrUnknown):
			t.Fatalf("loser %d: %v", i, e)
		}
	}
	p := s.Peers()
	if win < 0 || len(p) != 1 || p[0].FP != fpOf(fmt.Sprint("client", win)) {
		t.Fatalf("winner %d peers %+v", win, p)
	}
	if w := Writes.Load(); w != 2 {
		t.Fatalf("%d file writes in the race; only the winner's two (pending.json, peers.json) are allowed", w)
	}
	noPending(t, s)
}

func noPending(t *testing.T, s *Store) {
	t.Helper()
	var pend []Pending
	if err := readJSON(s.path("pending.json"), &pend); err != nil || len(pend) != 0 {
		t.Fatalf("the consumed token is back or pending.json is unreadable: %+v %v", pend, err)
	}
}

func TestP4_ConcurrentRedemptionOneWinner(t *testing.T)   { concurrent(t, 2) }
func TestP4_ConcurrentRedemptionManyClients(t *testing.T) { concurrent(t, 32) }

func TestP4_ConcurrentRedemptionAcrossProcesses(t *testing.T) {
	s, id, secret := newStore(t)
	const n = 4
	at := time.Now().Add(1500 * time.Millisecond).UnixNano()
	var outs [n]bytes.Buffer
	var cmds [n]*exec.Cmd
	for i := range n {
		c := exec.Command(os.Args[0], "-test.run=^TestHelperChild$")
		c.Env = append(os.Environ(), "PAIRING_CHILD=1", "PAIRING_DIR="+s.Dir, "PAIRING_ID="+id, "PAIRING_SECRET="+hex.EncodeToString(secret),
			"PAIRING_NAME="+fmt.Sprint("proc", i), "PAIRING_AT="+strconv.FormatInt(at, 10))
		c.Stdout = &outs[i]
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = c
	}
	won, winner := 0, -1
	for i := range n {
		if err := cmds[i].Wait(); err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, outs[i].String())
		}
		if strings.Contains(outs[i].String(), "RESULT enrolled") {
			won++
			winner = i
		} else if !strings.Contains(outs[i].String(), "RESULT unknown pairing id") {
			t.Fatalf("child %d: %q", i, outs[i].String())
		}
	}
	if p := s.Peers(); won != 1 || len(p) != 1 || p[0].FP != fpOf(fmt.Sprint("proc", winner)) {
		t.Fatalf("winners %d (process %d) peers %+v", won, winner, p)
	}
	noPending(t, s)
}

func TestHelperChild(t *testing.T) {
	if os.Getenv("PAIRING_CHILD") != "1" {
		t.Skip("helper process")
	}
	secret, _ := hex.DecodeString(os.Getenv("PAIRING_SECRET"))
	at, _ := strconv.ParseInt(os.Getenv("PAIRING_AT"), 10, 64)
	s := &Store{Dir: os.Getenv("PAIRING_DIR"), ServerFP: serverFP, Now: func() time.Time { return t0 }}
	name := os.Getenv("PAIRING_NAME")
	time.Sleep(time.Until(time.Unix(0, at)))
	err := s.Redeem(os.Getenv("PAIRING_ID"), MAC(secret, fpOf(name), serverFP), fpOf(name), name, "a")
	if err == nil {
		fmt.Println("RESULT enrolled")
	} else {
		fmt.Println("RESULT " + err.Error())
	}
}

// --- revocation ---

type host struct {
	cert tls.Certificate
	fp   string
}

func newHost(t *testing.T) host {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "spike"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return host{tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, hex.EncodeToString(sum[:])}
}

func fpState(cs *tls.ConnectionState) string {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		return ""
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

type verifyRec struct{ resumed, ok bool }

type gw struct {
	addr     string
	handled  atomic.Int64
	mu       sync.Mutex
	verified []verifyRec
}

func (g *gw) lastVerify() (verifyRec, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.verified) == 0 {
		return verifyRec{}, false
	}
	return g.verified[len(g.verified)-1], true
}

// newGateway: section 3.2 rules. recheck=false removes the per-request pin read; tickets=true re-enables session tickets.
func newGateway(t *testing.T, s *Store, srv host, recheck, tickets bool) *gw {
	t.Helper()
	g := &gw{}
	cfg := &tls.Config{Certificates: []tls.Certificate{srv.cert}, MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert,
		SessionTicketsDisabled: !tickets, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			ok := s.Pinned(fpState(&cs))
			g.mu.Lock()
			g.verified = append(g.verified, verifyRec{cs.DidResume, ok})
			g.mu.Unlock()
			if !ok {
				return errors.New("not pinned")
			}
			return nil
		}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if recheck && !s.Pinned(fpState(r.TLS)) {
			http.Error(w, "unpaired", http.StatusForbidden)
			return
		}
		g.handled.Add(1)
		w.WriteHeader(http.StatusOK)
	})}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	g.addr = ln.Addr().String()
	return g
}

func client(c host, keepAlive bool) *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: !keepAlive, TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{c.cert}, MinVersion: tls.VersionTLS13, InsecureSkipVerify: true,
		ClientSessionCache: tls.NewLRUClientSessionCache(8)}}}
}

func get(t *testing.T, c *http.Client, addr string) (status int, reused, resumed bool, err error) {
	t.Helper()
	req, _ := http.NewRequest("GET", "https://"+addr+"/", nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}))
	resp, err := c.Do(req)
	if err != nil {
		return 0, reused, false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, reused, resp.TLS != nil && resp.TLS.DidResume, nil
}

func pairedRig(t *testing.T) (*Store, host, host) {
	s := &Store{Dir: t.TempDir(), ServerFP: "", Now: time.Now}
	srv, cl := newHost(t), newHost(t)
	s.ServerFP = srv.fp
	id, secret, err := s.Start(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Redeem(id, MAC(secret, cl.fp, srv.fp), cl.fp, "client", "a"); err != nil {
		t.Fatal(err)
	}
	return s, srv, cl
}

func TestP5_OpenConnectionRefusedAfterUnpair(t *testing.T) {
	for _, recheck := range []bool{true, false} {
		t.Run(fmt.Sprint("recheck=", recheck), func(t *testing.T) {
			s, srv, cl := pairedRig(t)
			g := newGateway(t, s, srv, recheck, false)
			c := client(cl, true)
			if st, _, _, err := get(t, c, g.addr); err != nil || st != 200 {
				t.Fatalf("request 1: %d %v", st, err)
			}
			if err := s.Unpair(cl.fp); err != nil {
				t.Fatal(err)
			}
			st, reused, _, err := get(t, c, g.addr)
			if !reused {
				t.Fatal("request 2 did not reuse the open connection; the test proves nothing")
			}
			if recheck {
				if err != nil || st != 403 || g.handled.Load() != 1 {
					t.Fatalf("status %d err %v handled %d", st, err, g.handled.Load())
				}
			} else if err != nil || st != 200 || g.handled.Load() != 2 {
				t.Fatalf("control without the per-request recheck: status %d err %v handled %d (the open connection should have been served)", st, err, g.handled.Load())
			}
		})
	}
}

func TestP6_ResumedConnectionRefusedAfterUnpair(t *testing.T) {
	s, srv, cl := pairedRig(t)
	g := newGateway(t, s, srv, true, false)
	c := client(cl, false)
	for i := range 2 {
		st, _, resumed, err := get(t, c, g.addr)
		if err != nil || st != 200 || resumed {
			t.Fatalf("connection %d: %d resumed=%v %v", i+1, st, resumed, err)
		}
	}
	if err := s.Unpair(cl.fp); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := get(t, c, g.addr)
	if err == nil {
		t.Fatal("new connection after unpair was served")
	}
	t.Logf("client error: %v", err)
	if g.handled.Load() != 2 {
		t.Fatalf("handled %d", g.handled.Load())
	}
}

func TestP6_ResumptionWouldStillBeRefusedByVerifyConnection(t *testing.T) {
	s, srv, cl := pairedRig(t)
	g := newGateway(t, s, srv, true, true)
	c := client(cl, false)
	if st, _, resumed, err := get(t, c, g.addr); err != nil || st != 200 || resumed {
		t.Fatalf("connection 1: %d %v %v", st, resumed, err)
	}
	st, _, resumed, err := get(t, c, g.addr)
	if err != nil || st != 200 || !resumed {
		t.Fatalf("connection 2 must resume for this control to mean anything: %d resumed=%v %v", st, resumed, err)
	}
	if err := s.Unpair(cl.fp); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = get(t, c, g.addr)
	if err == nil {
		t.Fatal("resumed connection after unpair was served")
	}
	if v, ok := g.lastVerify(); !ok || !v.resumed || v.ok {
		t.Fatalf("the post-unpair attempt must have reached VerifyConnection as a resumption and failed the pin check: %+v %v", v, ok)
	}
	t.Logf("client error: %v", err)
	if g.handled.Load() != 2 {
		t.Fatalf("handled %d", g.handled.Load())
	}
}

var _ = net.Listen
