package remote

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	StatePortNotBound = "service running, port not bound"
	stateStarting     = "starting"
	stateListening    = "listening"
)

var ErrPortHeld = errors.New("remote: port is held by another process")

// Limits are the gateway's bounds (plan section 3.2); tests shorten them.
type Limits struct {
	HandshakeTimeout, HeaderTimeout, ReadTimeout, WriteTimeout, IdleTimeout time.Duration
	MaxBody                                                                 int64
	MaxPinnedConns, MaxPeerConcurrent, MaxPeerPerMinute                     int
	MaxHandshaking, MaxUnenrolled                                           int
	UnenrolledLife, PendingPoll, BindRetry, BindGiveUp                      time.Duration
	StallWindow                                                             time.Duration
	MaxStallAddrs                                                           int
	// PairInterval is the gateway-wide gap between pairing attempts; negative disables it.
	PairInterval time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		HandshakeTimeout: 5 * time.Second, HeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		MaxBody:        80 << 10,
		MaxPinnedConns: 64, MaxPeerConcurrent: 8, MaxPeerPerMinute: 120,
		MaxHandshaking: 16, MaxUnenrolled: 4,
		UnenrolledLife: 15 * time.Second, PendingPoll: time.Second,
		BindRetry: 10 * time.Second, BindGiveUp: 60 * time.Second,
		PairInterval: time.Second,
		StallWindow:  time.Minute, MaxStallAddrs: 256,
	}
}

type GatewayConfig struct {
	StateDir string
	Host     Host
	Bind     string
	Port     int
	// Alias is the name this host announces to a host that pairs with it.
	Alias string
	Peers *Peers
	// Handler serves requests from pinned peers; nil answers 404.
	Handler http.Handler
	// Limits zero value means DefaultLimits.
	Limits Limits
	Now    func() time.Time
	Listen func(network, addr string) (net.Listener, error)
	Log    func(format string, args ...any)
}

type connInfo struct {
	fp         string
	unenrolled bool
}

type gwConn struct {
	*tls.Conn
	g      *Gateway
	info   *connInfo
	once   sync.Once
	closed atomic.Bool
	mu     sync.Mutex
	timer  *time.Timer
}

// Close drops the socket at once, with no close-notify (which can block for
// seconds on a peer that stopped reading), and frees the pool slot only after
// the socket is closed.
func (c *gwConn) Close() error {
	var err error
	c.once.Do(func() {
		c.mu.Lock()
		t := c.timer
		c.mu.Unlock()
		if t != nil {
			t.Stop()
		}
		c.closed.Store(true)
		err = c.Conn.NetConn().Close()
		c.g.forget(c)
	})
	return err
}

type peerState struct {
	stamps   []time.Time
	inflight int
}

type Gateway struct {
	cfg    GatewayConfig
	base   *tls.Config
	ready  chan struct{}
	addr   net.Addr
	state  atomic.Value
	quit   chan struct{}
	stalls *stallLimiter

	mu       sync.Mutex
	raw      map[net.Conn]struct{}
	conns    map[*gwConn]struct{}
	pinned   int
	unen     int
	peerSt   map[string]*peerState
	accepted chan *gwConn
	lastPair time.Time
}

func NewGateway(c GatewayConfig) *Gateway {
	if c.Limits == (Limits{}) {
		c.Limits = DefaultLimits()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Listen == nil {
		c.Listen = net.Listen
	}
	if c.Log == nil {
		c.Log = func(string, ...any) {}
	}
	if c.Handler == nil {
		c.Handler = http.NotFoundHandler()
	}
	if c.Alias == "" {
		c.Alias = DefaultAlias()
	}
	g := &Gateway{
		cfg: c, ready: make(chan struct{}), quit: make(chan struct{}),
		raw: map[net.Conn]struct{}{}, conns: map[*gwConn]struct{}{}, peerSt: map[string]*peerState{},
		accepted: make(chan *gwConn),
		stalls:   newStallLimiter(c.Limits.StallWindow, c.Limits.MaxStallAddrs),
	}
	g.base = &tls.Config{
		Certificates:           []tls.Certificate{c.Host.Certificate},
		MinVersion:             tls.VersionTLS13,
		ClientAuth:             tls.RequireAnyClientCert,
		SessionTicketsDisabled: true,
		InsecureSkipVerify:     true,
	}
	// One key for every connection's config, so SessionTicketsDisabled is the
	// only thing standing between a client and resumption.
	var key [32]byte
	rand.Read(key[:])
	g.base.SetSessionTicketKeys([][32]byte{key})
	g.state.Store(stateStarting)
	return g
}

func (g *Gateway) Ready() <-chan struct{} { return g.ready }
func (g *Gateway) Addr() net.Addr         { return g.addr }
func (g *Gateway) State() string          { return g.state.Load().(string) }

type peerKey struct{}

// PeerFrom is the pinned peer a handler's request came from.
func PeerFrom(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}

type connKey struct{}

// Serve runs the gateway until ctx ends (nil) or it cannot run (error).
func (g *Gateway) Serve(ctx context.Context) error {
	if err := ensureDir(g.cfg.StateDir); err != nil {
		return err
	}
	release, err := acquireGatewayLock(g.cfg.StateDir)
	if err != nil {
		return err
	}
	defer release()
	if _, err := EnsureToken(g.cfg.StateDir); err != nil {
		return err
	}
	ln, err := g.bind(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	g.addr = ln.Addr()
	g.state.Store(stateListening)
	close(g.ready)
	g.cfg.Log("gateway listening on %s, fingerprint %s", g.addr, g.cfg.Host.Fingerprint)

	lim := g.cfg.Limits
	srv := &http.Server{
		Handler:           http.HandlerFunc(g.serveHTTP),
		ReadHeaderTimeout: lim.HeaderTimeout,
		ReadTimeout:       lim.ReadTimeout,
		WriteTimeout:      lim.WriteTimeout,
		IdleTimeout:       lim.IdleTimeout,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, c)
		},
	}
	var wg sync.WaitGroup
	wg.Add(3)
	srvDone := make(chan struct{})
	go func() { defer wg.Done(); srv.Serve(chanListener{g}); close(srvDone) }()
	go func() { defer wg.Done(); g.sweepUnenrolled() }()
	go func() { defer wg.Done(); g.acceptLoop(ln) }()

	select {
	case <-ctx.Done():
	case <-srvDone:
	}
	close(g.quit)
	ln.Close()
	srv.Close()
	g.mu.Lock()
	for c := range g.raw {
		c.Close()
	}
	for c := range g.conns {
		go c.Close()
	}
	g.mu.Unlock()
	wg.Wait()
	return nil
}

func (g *Gateway) bind(ctx context.Context) (net.Listener, error) {
	addr := net.JoinHostPort(g.cfg.Bind, strconv.Itoa(g.cfg.Port))
	var heldSince time.Time
	for {
		ln, err := g.cfg.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		switch {
		case errors.Is(err, syscall.EADDRINUSE):
			if heldSince.IsZero() {
				heldSince = time.Now()
			}
			if time.Since(heldSince) >= g.cfg.Limits.BindGiveUp {
				return nil, fmt.Errorf("%w: %s", ErrPortHeld, addr)
			}
		case errors.Is(err, syscall.EADDRNOTAVAIL):
			heldSince = time.Time{}
		default:
			return nil, fmt.Errorf("remote: listen %s: %w", addr, err)
		}
		g.state.Store(StatePortNotBound)
		g.cfg.Log("cannot bind %s: %v; retry in %v", addr, err, g.cfg.Limits.BindRetry)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(g.cfg.Limits.BindRetry):
		}
	}
}

func (g *Gateway) acceptLoop(ln net.Listener) {
	slots := make(chan struct{}, g.cfg.Limits.MaxHandshaking)
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-g.quit:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			g.cfg.Log("accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		g.mu.Lock()
		g.raw[c] = struct{}{}
		g.mu.Unlock()
		go g.handshake(c, slots)
	}
}

func (g *Gateway) handshake(raw net.Conn, slots chan struct{}) {
	info := &connInfo{}
	tc := tls.Server(raw, g.tlsConfig(info))
	raw.SetDeadline(time.Now().Add(g.cfg.Limits.HandshakeTimeout))
	err := tc.Handshake()
	g.mu.Lock()
	delete(g.raw, raw)
	g.mu.Unlock()
	<-slots
	if err != nil {
		g.noteStall(raw, err)
		raw.Close()
		return
	}
	raw.SetDeadline(time.Time{})
	c := &gwConn{Conn: tc, g: g, info: info}
	if info.unenrolled {
		c.mu.Lock()
		c.timer = time.AfterFunc(g.cfg.Limits.UnenrolledLife, func() { c.Close() })
		c.mu.Unlock()
	}
	if !g.admit(c) {
		c.Close()
		return
	}
	select {
	case g.accepted <- c:
	case <-g.quit:
		c.Close()
	}
}

func (g *Gateway) admit(c *gwConn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.quit:
		return false
	default:
	}
	if c.closed.Load() {
		return false
	}
	if c.info.unenrolled {
		if g.unen >= g.cfg.Limits.MaxUnenrolled {
			return false
		}
		g.unen++
	} else {
		if g.pinned >= g.cfg.Limits.MaxPinnedConns {
			return false
		}
		g.pinned++
	}
	g.conns[c] = struct{}{}
	return true
}

func (g *Gateway) forget(c *gwConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.conns[c]; !ok {
		return
	}
	delete(g.conns, c)
	if c.info.unenrolled {
		g.unen--
	} else {
		g.pinned--
	}
}

func (g *Gateway) sweepUnenrolled() {
	t := time.NewTicker(g.cfg.Limits.PendingPoll)
	defer t.Stop()
	for {
		select {
		case <-g.quit:
			return
		case <-t.C:
		}
		g.mu.Lock()
		var victims []*gwConn
		if g.unen > 0 {
			for c := range g.conns {
				if c.info.unenrolled {
					victims = append(victims, c)
				}
			}
		}
		g.mu.Unlock()
		if len(victims) > 0 && !g.pairingPending() {
			for _, c := range victims {
				c.Close()
			}
		}
	}
}

type chanListener struct{ g *Gateway }

func (l chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.g.accepted:
		return c, nil
	case <-l.g.quit:
		return nil, net.ErrClosed
	}
}
func (l chanListener) Close() error   { return nil }
func (l chanListener) Addr() net.Addr { return l.g.addr }

func (g *Gateway) tlsConfig(info *connInfo) *tls.Config {
	c := g.base.Clone()
	c.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no client certificate")
		}
		fp := Fingerprint(cs.PeerCertificates[0])
		_, ok, err := g.cfg.Peers.Lookup(fp)
		if err != nil {
			g.cfg.Log("peers table unreadable, refusing handshake: %v", err)
			return errors.New("peers table unreadable")
		}
		info.fp = fp
		if ok {
			return nil
		}
		if g.pairingPending() {
			info.unenrolled = true
			return nil
		}
		return errors.New("client certificate is not pinned")
	}
	return c
}

// pairingPending reads pending.json through loadPending; a file that is
// missing, has the wrong mode or does not parse admits no unenrolled client.
func (g *Gateway) pairingPending() bool {
	es, err := loadPending(g.cfg.StateDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			g.cfg.Log("pending.json refused: %v", err)
		}
		return false
	}
	now := g.cfg.Now()
	for _, e := range es {
		if e.live(now) {
			return true
		}
	}
	return false
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	c := r.Context().Value(connKey{}).(*gwConn)
	if c.info.unenrolled {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/pair" {
			g.handlePair(w, r, c)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p, ok, err := g.cfg.Peers.Lookup(c.info.fp)
	if err != nil || !ok {
		w.Header().Set("Connection", "close")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	done, status := g.enter(c.info.fp)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	defer done()
	lim := g.cfg.Limits.MaxBody
	if r.ContentLength > lim {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, lim)
	if r.Method == http.MethodPost && r.URL.Path == "/v1/pair" {
		g.handlePair(w, r, c)
		return
	}
	g.cfg.Handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, p)))
}

func (g *Gateway) enter(fp string) (func(), int) {
	now := g.cfg.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	ps := g.peerSt[fp]
	if ps == nil {
		ps = &peerState{}
		g.peerSt[fp] = ps
	}
	keep := ps.stamps[:0]
	for _, t := range ps.stamps {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	ps.stamps = keep
	if len(ps.stamps) >= g.cfg.Limits.MaxPeerPerMinute {
		return nil, http.StatusTooManyRequests
	}
	ps.stamps = append(ps.stamps, now)
	if ps.inflight >= g.cfg.Limits.MaxPeerConcurrent {
		return nil, http.StatusTooManyRequests
	}
	ps.inflight++
	return func() {
		g.mu.Lock()
		ps.inflight--
		g.mu.Unlock()
	}, 0
}

// acquireGatewayLock takes gateway.lock without waiting and records this
// pid in it so a second gateway can name the holder.
func acquireGatewayLock(stateDir string) (func(), error) {
	f, err := openLockFile(stateDir, "gateway.lock")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "gateway.lock")
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		defer f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			b, _ := io.ReadAll(io.LimitReader(f, 32))
			holder := strings.TrimSpace(string(b))
			if holder == "" {
				holder = "unknown"
			}
			return nil, fmt.Errorf("remote: another gateway holds %s (pid %s)", path, holder)
		}
		return nil, err
	}
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return func() { f.Close() }, nil
}

// EnsureToken returns the gateway credential, creating it on first use with
// an exclusive create (a temp file hard-linked into place, so the file never
// exists empty) and keeping it across restarts.
func EnsureToken(stateDir string) (string, error) {
	if err := ensureDir(stateDir); err != nil {
		return "", err
	}
	path := filepath.Join(stateDir, "gateway.token")
	for i := 0; i < 3; i++ {
		b, err := readChecked(path, 0o600)
		if err == nil {
			tok := strings.TrimSpace(string(b))
			if len(tok) < 32 || strings.Trim(tok, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
				return "", fmt.Errorf("remote: %s is not a gateway token", path)
			}
			return tok, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		tok := base64.RawURLEncoding.EncodeToString(raw)
		tmp, err := os.CreateTemp(stateDir, "gateway.token.tmp*")
		if err != nil {
			return "", err
		}
		_, werr := tmp.WriteString(tok + "\n")
		cerr := errors.Join(werr, tmp.Chmod(0o600), tmp.Sync(), tmp.Close())
		lerr := error(nil)
		if cerr == nil {
			lerr = os.Link(tmp.Name(), path)
		}
		os.Remove(tmp.Name())
		if cerr != nil {
			return "", cerr
		}
		if lerr == nil {
			return tok, nil
		}
		if !errors.Is(lerr, os.ErrExist) {
			return "", lerr
		}
	}
	return "", fmt.Errorf("remote: could not create %s", path)
}
