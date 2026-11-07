package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", args...)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type gwEnv struct {
	t        *testing.T
	cfgDir   string
	stateDir string
	host     Host
	peers    *Peers
	gw       *Gateway
	hits     atomic.Int32
	logs     *logBuf
	done     chan error
	cancel   context.CancelFunc
	nPeers   int
}

func newGateway(t *testing.T, mut func(*GatewayConfig)) *gwEnv {
	t.Helper()
	dir := newDir(t)
	host, err := LoadOrCreateHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &gwEnv{t: t, cfgDir: dir, stateDir: filepath.Join(t.TempDir(), "state"), host: host, peers: NewPeers(dir), logs: &logBuf{}}
	cfg := GatewayConfig{
		StateDir: e.stateDir, Host: host, Bind: "127.0.0.1", Port: 0, Peers: e.peers,
		Limits: DefaultLimits(), Log: e.logs.logf,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			e.hits.Add(1)
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	if mut != nil {
		mut(&cfg)
	}
	e.gw = NewGateway(cfg)
	return e
}

func (e *gwEnv) start() *gwEnv {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- e.gw.Serve(ctx) }()
	select {
	case <-e.gw.Ready():
	case err := <-e.done:
		e.t.Fatalf("Serve returned before ready: %v\nlogs:\n%s", err, e.logs)
	case <-time.After(5 * time.Second):
		e.t.Fatalf("gateway not ready\nlogs:\n%s", e.logs)
	}
	e.t.Cleanup(e.stop)
	return e
}

func (e *gwEnv) stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		e.t.Error("gateway did not stop")
	}
	e.cancel = nil
}

func startGateway(t *testing.T, mut func(*GatewayConfig)) *gwEnv {
	t.Helper()
	return newGateway(t, mut).start()
}

func (e *gwEnv) addr() string { return e.gw.Addr().String() }

func (e *gwEnv) newClient(pinned bool) Host {
	e.t.Helper()
	h, err := LoadOrCreateHost(newDir(e.t))
	if err != nil {
		e.t.Fatal(err)
	}
	if pinned {
		e.nPeers++
		err := e.peers.Upsert(Peer{Alias: "c" + strconv.Itoa(e.nPeers), Address: "192.0.2.1:7745", Fingerprint: h.Fingerprint})
		if err != nil {
			e.t.Fatal(err)
		}
	}
	return h
}

func clientTLS(h Host, max uint16) *tls.Config {
	c := &tls.Config{Certificates: []tls.Certificate{h.Certificate}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	if max != 0 {
		c.MaxVersion = max
	}
	return c
}

func (e *gwEnv) dial(cfg *tls.Config) (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", e.addr(), cfg)
}

func roundTrip(c net.Conn) (int, error) {
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, "GET /x HTTP/1.1\r\nHost: g\r\n\r\n"); err != nil {
		return 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (e *gwEnv) httpClient(h Host) *http.Client {
	tr := &http.Transport{TLSClientConfig: clientTLS(h, 0), MaxIdleConnsPerHost: 1}
	e.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func (e *gwEnv) do(c *http.Client, method string, body io.Reader, length int64) (*http.Response, error) {
	req, err := http.NewRequest(method, "https://"+e.addr()+"/x", body)
	if err != nil {
		return nil, err
	}
	if length >= 0 {
		req.ContentLength = length
	}
	resp, err := c.Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return resp, err
}

func writePending(t *testing.T, stateDir string, expires time.Time) {
	t.Helper()
	d := filepath.Join(stateDir, "remote")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`[{"id":"p1","secret":"s","expires_at":%q}]`, expires.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(d, "pending.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUnpinnedClientRefusedBeforeHTTP(t *testing.T) {
	e := startGateway(t, nil)
	stranger := e.newClient(false)
	c, err := e.dial(clientTLS(stranger, 0))
	if err == nil {
		defer c.Close()
		if st, err := roundTrip(c); err == nil {
			t.Fatalf("unpinned client got HTTP status %d", st)
		}
	}
	pinned := e.newClient(true)
	c2, err := e.dial(clientTLS(pinned, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if st, err := roundTrip(c2); err != nil || st != http.StatusNoContent {
		t.Fatalf("pinned client: %d %v", st, err)
	}
	if n := e.hits.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1 (the pinned request only)", n)
	}
}

func TestTLS12Refused(t *testing.T) {
	e := startGateway(t, nil)
	pinned := e.newClient(true)
	c, err := e.dial(clientTLS(pinned, tls.VersionTLS12))
	if err == nil {
		c.Close()
		t.Fatal("a TLS 1.2 client completed a handshake")
	}
	if e.hits.Load() != 0 {
		t.Fatal("handler ran for a TLS 1.2 client")
	}
	c13, err := e.dial(clientTLS(pinned, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c13.Close()
	if v := c13.ConnectionState().Version; v != tls.VersionTLS13 {
		t.Fatalf("negotiated %x, want TLS 1.3", v)
	}
}

func TestUnpairRefusesOpenConnection(t *testing.T) {
	e := startGateway(t, nil)
	h := e.newClient(true)
	cl := e.httpClient(h)
	if resp, err := e.do(cl, "GET", nil, -1); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("before unpair: %v %v", resp, err)
	}
	if err := e.peers.Remove("c1"); err != nil {
		t.Fatal(err)
	}
	var reused bool
	req, _ := http.NewRequest("GET", "https://"+e.addr()+"/x", nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused },
	}))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("after unpair: %v", err)
	}
	resp.Body.Close()
	if !reused {
		t.Fatal("second request used a new connection; the test must exercise the open one")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("request on the open connection after unpair: status %d, want 403", resp.StatusCode)
	}
	if n := e.hits.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
}

func TestNoSessionResumption(t *testing.T) {
	e := startGateway(t, nil)
	h := e.newClient(true)
	cache := tls.NewLRUClientSessionCache(8)
	cfg := clientTLS(h, 0)
	cfg.ClientSessionCache = cache
	for i := 0; i < 2; i++ {
		c, err := e.dial(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if st, err := roundTrip(c); err != nil || st != http.StatusNoContent {
			t.Fatalf("connection %d: %d %v", i, st, err)
		}
		if c.ConnectionState().DidResume {
			t.Fatalf("connection %d resumed a session", i)
		}
		c.Close()
	}
	if err := e.peers.Remove("c1"); err != nil {
		t.Fatal(err)
	}
	c, err := e.dial(cfg)
	if err == nil {
		defer c.Close()
		if st, err := roundTrip(c); err == nil {
			t.Fatalf("unpaired client with a session cache got status %d", st)
		}
	}
}

func TestGatewayLimits(t *testing.T) {
	t.Run("body", func(t *testing.T) {
		var readErr atomic.Value
		e := startGateway(t, func(c *GatewayConfig) {
			c.Limits.MaxBody = 1024
			c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					readErr.Store(err)
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
		})
		cl := e.httpClient(e.newClient(true))
		if resp, err := e.do(cl, "POST", bytes.NewReader(make([]byte, 1025)), 1025); err != nil || resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("declared 1025 bytes: %v %v", resp, err)
		}
		if resp, err := e.do(cl, "POST", bytes.NewReader(make([]byte, 1024)), 1024); err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("1024 bytes: %v %v", resp, err)
		}
		resp, err := e.do(cl, "POST", io.NopCloser(bytes.NewReader(make([]byte, 4000))), -1)
		if err != nil || resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("chunked 4000 bytes: %v %v", resp, err)
		}
		var mbe *http.MaxBytesError
		if err, _ := readErr.Load().(error); !errors.As(err, &mbe) {
			t.Fatalf("handler read error %v, want *http.MaxBytesError", err)
		}
	})

	t.Run("concurrency", func(t *testing.T) {
		release := make(chan struct{})
		var inside atomic.Int32
		e := startGateway(t, func(c *GatewayConfig) {
			c.Limits.MaxPeerConcurrent = 2
			c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inside.Add(1)
				<-release
				w.WriteHeader(http.StatusNoContent)
			})
		})
		h := e.newClient(true)
		other := e.newClient(true)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.do(e.httpClient(h), "GET", nil, -1)
			}()
		}
		deadline := time.Now().Add(3 * time.Second)
		for inside.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if inside.Load() != 2 {
			t.Fatal("two requests never ran concurrently")
		}
		if resp, err := e.do(e.httpClient(h), "GET", nil, -1); err != nil || resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("third concurrent request: %v %v", resp, err)
		}
		go func() { time.Sleep(50 * time.Millisecond); close(release) }()
		if resp, err := e.do(e.httpClient(other), "GET", nil, -1); err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("another peer was limited by the first: %v %v", resp, err)
		}
		wg.Wait()
	})

	t.Run("rate", func(t *testing.T) {
		var now atomic.Int64
		now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
		e := startGateway(t, func(c *GatewayConfig) {
			c.Limits.MaxPeerPerMinute = 3
			c.Now = func() time.Time { return time.Unix(0, now.Load()) }
		})
		cl := e.httpClient(e.newClient(true))
		for i := 0; i < 3; i++ {
			if resp, err := e.do(cl, "GET", nil, -1); err != nil || resp.StatusCode != http.StatusNoContent {
				t.Fatalf("request %d: %v %v", i, resp, err)
			}
		}
		if resp, err := e.do(cl, "GET", nil, -1); err != nil || resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("fourth request in a minute: %v %v", resp, err)
		}
		now.Add(int64(61 * time.Second))
		if resp, err := e.do(cl, "GET", nil, -1); err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("after the window: %v %v", resp, err)
		}
	})
}

func TestSlowBodyClosed(t *testing.T) {
	const readTimeout = 400 * time.Millisecond
	var handlerErr atomic.Value
	e := startGateway(t, func(c *GatewayConfig) {
		c.Limits.ReadTimeout = readTimeout
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				handlerErr.Store(err)
			}
		})
	})
	c, err := e.dial(clientTLS(e.newClient(true), 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "POST /x HTTP/1.1\r\nHost: g\r\nContent-Length: 1000\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(40 * time.Millisecond):
				if _, err := c.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()
	start := time.Now()
	c.SetReadDeadline(start.Add(readTimeout + 3*time.Second))
	_, err = io.Copy(io.Discard, c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the trickling client was still connected after %v", time.Since(start))
	}
	if d := time.Since(start); d > readTimeout+1500*time.Millisecond {
		t.Fatalf("cut after %v, want about %v", d, readTimeout)
	}
	if handlerErr.Load() == nil {
		t.Fatal("the handler's body read did not fail")
	}
}

func TestIdleConnectionClosed(t *testing.T) {
	e := startGateway(t, func(c *GatewayConfig) {
		c.Limits.IdleTimeout = 300 * time.Millisecond
		c.Limits.HeaderTimeout = 300 * time.Millisecond
	})
	h := e.newClient(true)
	closedWithin := func(c net.Conn, d time.Duration) bool {
		c.SetReadDeadline(time.Now().Add(d))
		_, err := io.Copy(io.Discard, c)
		var ne net.Error
		return !(errors.As(err, &ne) && ne.Timeout())
	}
	c, err := e.dial(clientTLS(h, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !closedWithin(c, 1500*time.Millisecond) {
		t.Fatal("a pinned connection that sends nothing stayed open")
	}
	c2, err := e.dial(clientTLS(h, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if st, err := roundTrip(c2); err != nil || st != http.StatusNoContent {
		t.Fatalf("%d %v", st, err)
	}
	if !closedWithin(c2, 1500*time.Millisecond) {
		t.Fatal("a keep-alive connection stayed open past the idle timeout")
	}
}

func TestStalledHandshakesBounded(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%v", pending), func(t *testing.T) {
			const hs = 600 * time.Millisecond
			e := startGateway(t, func(c *GatewayConfig) {
				c.Limits.HandshakeTimeout = hs
				c.Limits.MaxHandshaking = 16
			})
			if pending {
				writePending(t, e.stateDir, time.Now().Add(10*time.Minute))
			}
			pinned := e.newClient(true)
			var conns []net.Conn
			for i := 0; i < 100; i++ {
				c, err := net.DialTimeout("tcp", e.addr(), 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				conns = append(conns, c)
			}
			defer func() {
				for _, c := range conns {
					c.Close()
				}
			}()
			time.Sleep(150 * time.Millisecond)
			held := 0
			for _, c := range conns {
				c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
				if _, err := c.Read(make([]byte, 1)); err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						held++
					}
				}
			}
			if held > 16 {
				t.Fatalf("%d stalled handshakes held, want at most 16", held)
			}
			if held == 0 {
				t.Fatal("no connection was held; the bound is not being exercised")
			}
			deadline := time.Now().Add(hs + time.Second)
			var lastErr error
			for time.Now().Before(deadline) {
				c, err := e.dial(clientTLS(pinned, 0))
				if err == nil {
					st, rerr := roundTrip(c)
					c.Close()
					if rerr == nil && st == http.StatusNoContent {
						return
					}
					err = rerr
				}
				lastErr = err
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("pinned peer could not connect within %v: %v", hs+time.Second, lastErr)
		})
	}
}

func TestBindRetry(t *testing.T) {
	t.Run("address missing", func(t *testing.T) {
		var calls atomic.Int32
		var stateSeen atomic.Value
		var e *gwEnv
		e = newGateway(t, func(c *GatewayConfig) {
			c.Limits.BindRetry = 10 * time.Millisecond
			c.Limits.BindGiveUp = 30 * time.Millisecond
			c.Listen = func(network, addr string) (net.Listener, error) {
				if n := calls.Add(1); n <= 8 {
					if n == 2 {
						stateSeen.Store(e.gw.State())
					}
					return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EADDRNOTAVAIL)}
				}
				return net.Listen(network, addr)
			}
		})
		e.start()
		if calls.Load() != 9 {
			t.Fatalf("listen attempts = %d, want 9: a missing address is retried past the give-up time", calls.Load())
		}
		if got, _ := stateSeen.Load().(string); got != StatePortNotBound {
			t.Fatalf("State() while retrying = %q, want %q", got, StatePortNotBound)
		}
		if !strings.Contains(e.logs.String(), "retry") {
			t.Fatalf("no retry line in the log:\n%s", e.logs)
		}
		c, err := e.dial(clientTLS(e.newClient(true), 0))
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if e.gw.State() != "listening" {
			t.Fatalf("State() = %q after bind", e.gw.State())
		}
	})
	t.Run("port held", func(t *testing.T) {
		held, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		e := newGateway(t, func(c *GatewayConfig) {
			c.Port = held.Addr().(*net.TCPAddr).Port
			c.Limits.BindRetry = 10 * time.Millisecond
			c.Limits.BindGiveUp = 100 * time.Millisecond
		})
		done := make(chan error, 1)
		go func() { done <- e.gw.Serve(context.Background()) }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrPortHeld) {
				t.Fatalf("Serve = %v, want ErrPortHeld", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Serve kept retrying a held port")
		}
	})
}

func TestOneGatewayPerHost(t *testing.T) {
	first := startGateway(t, nil)
	second := newGateway(t, func(c *GatewayConfig) { c.StateDir = first.stateDir })
	err := second.gw.Serve(context.Background())
	if err == nil {
		t.Fatal("a second gateway on the same state dir ran")
	}
	for _, want := range []string{"gateway.lock", strconv.Itoa(os.Getpid())} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	fi, err := os.Stat(filepath.Join(first.stateDir, "gateway.lock"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("gateway.lock: %v %v", fi, err)
	}
	first.stop()
	third := newGateway(t, func(c *GatewayConfig) { c.StateDir = first.stateDir })
	third.start()
}

func TestPinnedRequestGets404WithoutHandler(t *testing.T) {
	e := startGateway(t, func(c *GatewayConfig) { c.Handler = nil })
	c, err := e.dial(clientTLS(e.newClient(true), 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if st, err := roundTrip(c); err != nil || st != http.StatusNotFound {
		t.Fatalf("%d %v, want 404", st, err)
	}
}

func TestUnenrolledAdmission(t *testing.T) {
	e := startGateway(t, nil)
	stranger := e.newClient(false)
	if c, err := e.dial(clientTLS(stranger, 0)); err == nil {
		_, rerr := roundTrip(c)
		c.Close()
		if rerr == nil {
			t.Fatal("unenrolled client admitted with no pairing pending")
		}
	}
	writePending(t, e.stateDir, time.Now().Add(10*time.Minute))
	c, err := e.dial(clientTLS(stranger, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if st, err := roundTrip(c); err != nil || st != http.StatusForbidden {
		t.Fatalf("unenrolled request: %d %v, want 403", st, err)
	}
	if e.hits.Load() != 0 {
		t.Fatal("handler ran for an unenrolled client")
	}
}

func TestEnsureTokenKeptAcrossRestarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	a, err := EnsureToken(dir)
	if err != nil || len(a) < 32 {
		t.Fatalf("EnsureToken = %q, %v", a, err)
	}
	b, err := EnsureToken(dir)
	if err != nil || a != b {
		t.Fatalf("second EnsureToken = %q, %v; want the same token", b, err)
	}
	path := filepath.Join(dir, "gateway.token")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureToken(dir); !errors.Is(err, ErrMode) {
		t.Fatalf("0644 token: %v, want ErrMode", err)
	}
}
