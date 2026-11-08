package remote

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	pairTokenPrefix = "crosstalk-pair:v1:"
	pairMACLabel    = "crosstalk-pair-v1"
	PairTTL         = 10 * time.Minute
	maxPairBody     = 4 << 10
)

var (
	ErrPairingExpired = errors.New("remote: pairing expired")
	ErrPairingFailed  = errors.New("remote: pairing ended without a new peer")

	errPairUnknown  = errors.New("remote: unknown pairing id")
	errPairExpired  = errors.New("remote: pairing expired")
	errPairProof    = errors.New("remote: bad pairing proof")
	errPairAlias    = errors.New("remote: alias belongs to another peer")
	errPairBadField = errors.New("remote: bad pairing request")
)

// Pairing is one pending.json entry (plan sections 6 and 7). Alias, when set,
// is the name this host records for the host that redeems it.
type Pairing struct {
	ID        string    `json:"id"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
	Alias     string    `json:"alias,omitempty"`
}

func (p Pairing) live(now time.Time) bool { return now.Before(p.ExpiresAt) }

// PairToken is the one-line token `remote pair` prints.
type PairToken struct {
	Address     string
	Port        int
	Fingerprint string
	ID          string
	Secret      string
}

func (t PairToken) String() string {
	return pairTokenPrefix + t.Address + ":" + strconv.Itoa(t.Port) + ":" + t.Fingerprint + ":" + t.ID + ":" + t.Secret
}

func b64Len(s string, n int) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == n
}

// ParsePairToken reads the token from the right, so an IPv6 address in it is
// safe: the last three fields contain no colon.
func ParsePairToken(s string) (PairToken, error) {
	s = strings.TrimSpace(s)
	rest, ok := strings.CutPrefix(s, pairTokenPrefix)
	if !ok {
		return PairToken{}, errors.New("not a CrossTalk pairing token")
	}
	f := strings.Split(rest, ":")
	if len(f) < 5 {
		return PairToken{}, errors.New("pairing token is incomplete")
	}
	n := len(f)
	t := PairToken{Fingerprint: f[n-3], ID: f[n-2], Secret: f[n-1], Address: strings.Join(f[:n-4], ":")}
	port, err := strconv.Atoi(f[n-4])
	if err != nil || port < 1 || port > 65535 {
		return PairToken{}, errors.New("pairing token has a bad port")
	}
	t.Port = port
	if _, err := checkBindClass(t.Address); err != nil {
		return PairToken{}, fmt.Errorf("pairing token address: %w", err)
	}
	if !fingerprintRE.MatchString(t.Fingerprint) {
		return PairToken{}, errors.New("pairing token has a bad fingerprint")
	}
	if !b64Len(t.ID, 16) || !b64Len(t.Secret, 32) {
		return PairToken{}, errors.New("pairing token has a bad id or secret")
	}
	return t, nil
}

func computePairMAC(secret []byte, clientFP, serverFP string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(pairMACLabel + clientFP + serverFP))
	return h.Sum(nil)
}

func pendingDir(stateDir string) string  { return filepath.Join(stateDir, "remote") }
func pendingPath(stateDir string) string { return filepath.Join(pendingDir(stateDir), "pending.json") }

// loadPending is the one reader of pending.json. A missing file wraps
// os.ErrNotExist; a file with the wrong mode or bad JSON is an error.
func loadPending(stateDir string) ([]Pairing, error) {
	b, err := readChecked(pendingPath(stateDir), 0o600)
	if err != nil {
		return nil, err
	}
	var es []Pairing
	if err := json.Unmarshal(b, &es); err != nil {
		return nil, fmt.Errorf("remote: %s: %w", pendingPath(stateDir), err)
	}
	return es, nil
}

func savePending(stateDir string, es []Pairing) error {
	if es == nil {
		es = []Pairing{}
	}
	b, err := json.Marshal(es)
	if err != nil {
		return err
	}
	return writeAtomic(pendingPath(stateDir), append(b, '\n'), 0o600)
}

func lockPending(stateDir string) (func(), error) {
	if err := ensureDir(stateDir); err != nil {
		return nil, err
	}
	if err := ensureDir(pendingDir(stateDir)); err != nil {
		return nil, err
	}
	return lock(pendingDir(stateDir), "pending.lock")
}

// loadPendingOrEmpty treats a missing file as no pairings.
func loadPendingOrEmpty(stateDir string) ([]Pairing, error) {
	es, err := loadPending(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return es, err
}

func randB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// StartPairing records a new pending pairing, dropping expired ones.
func StartPairing(stateDir string, now time.Time, ttl time.Duration, alias string) (Pairing, error) {
	if alias != "" {
		if err := ValidAlias(alias); err != nil {
			return Pairing{}, err
		}
	}
	id, err := randB64(16)
	if err != nil {
		return Pairing{}, err
	}
	secret, err := randB64(32)
	if err != nil {
		return Pairing{}, err
	}
	unlock, err := lockPending(stateDir)
	if err != nil {
		return Pairing{}, err
	}
	defer unlock()
	es, err := loadPendingOrEmpty(stateDir)
	if err != nil {
		return Pairing{}, err
	}
	e := Pairing{ID: id, Secret: secret, ExpiresAt: now.Add(ttl).UTC(), Alias: alias}
	return e, savePending(stateDir, append(liveOnly(es, now), e))
}

func liveOnly(es []Pairing, now time.Time) []Pairing {
	var keep []Pairing
	for _, e := range es {
		if e.live(now) {
			keep = append(keep, e)
		}
	}
	return keep
}

// CancelPairing removes the entry and any expired ones.
func CancelPairing(stateDir, id string, now time.Time) error {
	unlock, err := lockPending(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	es, err := loadPendingOrEmpty(stateDir)
	if err != nil {
		return err
	}
	var keep []Pairing
	for _, e := range es {
		if e.ID != id && e.live(now) {
			keep = append(keep, e)
		}
	}
	if len(keep) == len(es) {
		return nil
	}
	return savePending(stateDir, keep)
}

type pairRequest struct {
	ID      string `json:"id"`
	Alias   string `json:"alias"`
	Address string `json:"address"`
	MAC     string `json:"mac"`
}

func validPeerAddress(s string) (string, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", err
	}
	a, err := checkBindClass(host)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("bad port %q", port)
	}
	return net.JoinHostPort(a.String(), port), nil
}

// redeemPairing runs under pending.lock from the read of pending.json to the
// end of the peers.json write (lock order: pending.lock, then peers.lock). It
// removes the entry before it enrolls the client, so a crash between the two
// burns the token and enrolls nobody. Any refusal leaves both files alone. The
// clock is read after pending.lock is held, and the alias check, the entry
// removal and the peers write all happen under peers.lock.
func redeemPairing(stateDir string, peers *Peers, clock func() time.Time, req pairRequest, clientFP, serverFP string) error {
	unlock, err := lockPending(stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	now := clock()
	es, err := loadPendingOrEmpty(stateDir)
	if err != nil {
		return err
	}
	at := -1
	for i, e := range es {
		if e.ID == req.ID {
			at = i
		}
	}
	if at < 0 {
		return errPairUnknown
	}
	e := es[at]
	if !e.live(now) {
		return errPairExpired
	}
	secret, err := base64.RawURLEncoding.DecodeString(e.Secret)
	if err != nil {
		return errPairProof
	}
	mac, err := base64.RawURLEncoding.DecodeString(req.MAC)
	if err != nil || !hmac.Equal(mac, computePairMAC(secret, clientFP, serverFP)) {
		return errPairProof
	}
	alias := e.Alias
	if alias == "" {
		alias = req.Alias
	}
	if err := ValidAlias(alias); err != nil {
		return fmt.Errorf("%w: alias: %v", errPairBadField, err)
	}
	addr, err := validPeerAddress(req.Address)
	if err != nil {
		return fmt.Errorf("%w: address: %v", errPairBadField, err)
	}
	err = peers.UpsertThen(Peer{Alias: alias, Address: addr, Fingerprint: clientFP, PairedAt: now.UTC(), PairingIDs: []string{e.ID}}, func() error {
		return savePending(stateDir, liveOnly(append(es[:at:at], es[at+1:]...), now))
	})
	if errors.Is(err, ErrAliasTaken) {
		return fmt.Errorf("%w: %q", errPairAlias, alias)
	}
	return err
}

// pairAllowed counts one attempt against the gateway-wide interval. A refused
// attempt does not restart the interval.
func (g *Gateway) pairAllowed() bool {
	now := g.cfg.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if iv := g.cfg.Limits.PairInterval; iv > 0 && !g.lastPair.IsZero() && !now.Before(g.lastPair) && now.Sub(g.lastPair) < iv {
		return false
	}
	g.lastPair = now
	return true
}

// handlePair serves POST /v1/pair for an unenrolled connection. The client's
// fingerprint is the handshake's, never the body's.
func (g *Gateway) handlePair(w http.ResponseWriter, r *http.Request, c *gwConn) {
	if !g.pairAllowed() {
		http.Error(w, "too many pairing attempts", http.StatusTooManyRequests)
		return
	}
	var req pairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPairBody)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	err := redeemPairing(g.cfg.StateDir, g.cfg.Peers, g.cfg.Now, req, c.info.fp, g.cfg.Host.Fingerprint)
	status := http.StatusOK
	switch {
	case err == nil:
	case errors.Is(err, errPairUnknown):
		status = http.StatusNotFound
	case errors.Is(err, errPairExpired):
		status = http.StatusGone
	case errors.Is(err, errPairProof):
		status = http.StatusForbidden
	case errors.Is(err, errPairAlias):
		status = http.StatusConflict
	case errors.Is(err, errPairBadField):
		status = http.StatusBadRequest
	default:
		status = http.StatusInternalServerError
	}
	if status != http.StatusOK {
		g.cfg.Log("pairing attempt refused (%d): %v", status, err)
		text := "internal error"
		if status != http.StatusInternalServerError {
			text = strings.TrimPrefix(err.Error(), "remote: ")
		}
		http.Error(w, text, status)
		return
	}
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"alias": g.cfg.Alias})
}

type JoinOptions struct {
	Host    Host
	Config  Config
	Peers   *Peers
	Token   PairToken
	Alias   string
	Timeout time.Duration
	// HandshakeTimeout bounds the TLS handshake alone; zero means 5 seconds.
	HandshakeTimeout time.Duration
}

// JoinGateway is `remote join`: it redeems the token on the other host's
// gateway, pinning that host's fingerprint from the token, then records the
// other host in Peers.
func JoinGateway(ctx context.Context, o JoinOptions) (Peer, error) {
	t := o.Token
	if t.Fingerprint == o.Host.Fingerprint {
		return Peer{}, errors.New("remote: the token belongs to this host")
	}
	if o.Alias != "" {
		if err := ValidAlias(o.Alias); err != nil {
			return Peer{}, err
		}
		ps, err := o.Peers.List()
		if err != nil {
			return Peer{}, err
		}
		for _, p := range ps {
			if p.Alias == o.Alias && p.Fingerprint != t.Fingerprint {
				return Peer{}, fmt.Errorf("%w: %q", ErrAliasTaken, o.Alias)
			}
		}
	}
	secret, err := base64.RawURLEncoding.DecodeString(t.Secret)
	if err != nil {
		return Peer{}, errors.New("remote: bad token secret")
	}
	addr := net.JoinHostPort(t.Address, strconv.Itoa(t.Port))
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	hsTimeout := o.HandshakeTimeout
	if hsTimeout <= 0 {
		hsTimeout = 5 * time.Second
	}
	tr := &http.Transport{
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: hsTimeout,
		TLSClientConfig: &tls.Config{
			Certificates:       []tls.Certificate{o.Host.Certificate},
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 || Fingerprint(cs.PeerCertificates[0]) != t.Fingerprint {
					return errors.New("the server's fingerprint does not match the token")
				}
				return nil
			},
		},
	}
	defer tr.CloseIdleConnections()
	body, err := json.Marshal(pairRequest{
		ID: t.ID, Alias: o.Config.Alias, Address: net.JoinHostPort(o.Config.Bind, strconv.Itoa(o.Config.Port)),
		MAC: base64.RawURLEncoding.EncodeToString(computePairMAC(secret, o.Host.Fingerprint, t.Fingerprint)),
	})
	if err != nil {
		return Peer{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, stalled := traceHandshake(ctx, addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+"/v1/pair", strings.NewReader(string(body)))
	if err != nil {
		return Peer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		if he := stalled(err); he != nil {
			return Peer{}, fmt.Errorf("remote: %w", he)
		}
		if strings.Contains(err.Error(), "bad certificate") {
			return Peer{}, fmt.Errorf("remote: %s is not waiting for a pairing; run crosstalk remote pair there first", addr)
		}
		return Peer{}, fmt.Errorf("remote: %s: %w", addr, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, maxPairBody))
	if resp.StatusCode != http.StatusOK {
		return Peer{}, fmt.Errorf("remote: %s refused the pairing (%d): %s", addr, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	var ans struct {
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(out, &ans); err != nil {
		return Peer{}, fmt.Errorf("remote: %s sent an unreadable answer: %w", addr, err)
	}
	name := o.Alias
	if name == "" {
		name = ans.Alias
	}
	if err := ValidAlias(name); err != nil {
		return Peer{}, fmt.Errorf("remote: the other host announced a bad alias: %w; pair again with --alias", err)
	}
	p := Peer{Alias: name, Address: addr, Fingerprint: t.Fingerprint, PairedAt: time.Now().UTC()}
	if err := o.Peers.Upsert(p); err != nil {
		return Peer{}, fmt.Errorf("%w (the other host has already enrolled this one; run pair and join again)", err)
	}
	return p, nil
}

type WaitOptions struct {
	StateDir string
	Peers    *Peers
	ID       string
	Poll     time.Duration
	Now      func() time.Time
}

// WaitPaired waits for the entry to be redeemed, to expire, or for ctx to end;
// the last two remove the entry. Redemption is seen as the entry leaving
// pending.json with a peer listing its pairing id.
func WaitPaired(ctx context.Context, o WaitOptions) (Peer, error) {
	if o.Poll <= 0 {
		o.Poll = 500 * time.Millisecond
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	for {
		es, err := loadPendingOrEmpty(o.StateDir)
		if err != nil {
			return Peer{}, err
		}
		var mine *Pairing
		for i := range es {
			if es[i].ID == o.ID {
				mine = &es[i]
			}
		}
		if mine == nil {
			unlock, err := lockPending(o.StateDir)
			if err != nil {
				return Peer{}, err
			}
			unlock()
			ps, err := o.Peers.List()
			if err != nil {
				return Peer{}, err
			}
			for _, p := range ps {
				if slices.Contains(p.PairingIDs, o.ID) {
					return p, nil
				}
			}
			return Peer{}, ErrPairingFailed
		}
		if !mine.live(now()) {
			if err := CancelPairing(o.StateDir, o.ID, now()); err != nil {
				return Peer{}, err
			}
			return Peer{}, ErrPairingExpired
		}
		select {
		case <-ctx.Done():
			CancelPairing(o.StateDir, o.ID, now())
			return Peer{}, ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

// GatewayRunning reports whether a gateway holds gateway.lock, without
// touching the pid recorded in it.
func GatewayRunning(stateDir string) (bool, error) {
	if _, err := os.Stat(stateDir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	f, err := openLockFile(stateDir, "gateway.lock")
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
