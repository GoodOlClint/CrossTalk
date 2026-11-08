package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"syscall"
	"time"
)

const (
	ReasonGatewayNotRunning = "gateway_not_running"
	ReasonTimeout           = "timeout"
	ReasonPinMismatch       = "tls_pin_mismatch"
	ReasonUnpairedByPeer    = "unpaired_by_peer"
	ReasonUnreachable       = "unreachable"
	ReasonInvalidListing    = "invalid_listing"

	MaxResponse = 1 << 20
	MaxRows     = 1000

	defaultClientTimeout = 5 * time.Second
)

var (
	errPinMismatch = errors.New("remote: peer certificate does not match the pinned fingerprint")
	errTooLarge    = errors.New("remote: response over the size cap")
)

// HandshakeHint is appended to every report of a stalled handshake.
const HandshakeHint = "a host firewall or network filter (for example an application firewall such as Little Snitch) on either host may be holding the connection"

// HandshakeError reports a TLS handshake that timed out or was closed by the
// peer before it finished.
type HandshakeError struct{ Addr string }

func (e *HandshakeError) Error() string {
	return "handshake timeout: no TLS handshake completed with " + e.Addr + "; " + HandshakeHint
}

// traceHandshake returns a context that records how the TLS handshake went,
// and a function that takes the request's error and returns a *HandshakeError
// when the handshake timed out or was closed before it finished.
func traceHandshake(ctx context.Context, addr string) (context.Context, func(error) error) {
	var mu sync.Mutex
	var started, done bool
	var hsErr error
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		TLSHandshakeStart: func() { mu.Lock(); started = true; mu.Unlock() },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { mu.Lock(); done, hsErr = true, err; mu.Unlock() },
	})
	return ctx, func(reqErr error) error {
		mu.Lock()
		defer mu.Unlock()
		if !started {
			return nil
		}
		cause := reqErr
		if done {
			cause = hsErr
		}
		if cause != nil && (handshakeCut(cause) || errors.Is(cause, context.DeadlineExceeded)) {
			return &HandshakeError{Addr: addr}
		}
		return nil
	}
}

// Error carries one of the Reason* codes.
type Error struct {
	Reason string
	Err    error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}
func (e *Error) Unwrap() error { return e.Err }

// Client dials one paired peer's gateway with this host's certificate and
// accepts only the peer's pinned fingerprint. Nothing is cached or reused
// across calls: every request is a full handshake, so a pin is checked each time.
type Client struct {
	host Host
	peer Peer
	// Timeout bounds one request when the caller's context has no tighter deadline.
	Timeout time.Duration
}

func NewClient(host Host, peer Peer) *Client {
	return &Client{host: host, peer: peer, Timeout: defaultClientTimeout}
}

func (c *Client) httpClient() (*http.Client, *http.Transport) {
	tr := &http.Transport{
		Proxy:               nil,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: c.Timeout,
		TLSClientConfig:     pinnedTLSConfig(c.host, c.peer, nil),
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, tr
}

// HTTPClient returns a client that dials only this peer's pinned
// fingerprint, with this host's certificate, TLS 1.3 only, one full handshake
// per request and no redirects. It sets no Timeout: callers bound it with a
// context.
func (c *Client) HTTPClient() *http.Client {
	hc, tr := c.httpClient()
	hc.Transport = capBody{stallWrap{tr, c.peer.Address}}
	return hc
}

// stallWrap turns a request that failed in the TLS handshake into the same
// *Error that Do returns, carrying a *HandshakeError.
type stallWrap struct {
	http.RoundTripper
	addr string
}

func (s stallWrap) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, stalled := traceHandshake(r.Context(), s.addr)
	resp, err := s.RoundTripper.RoundTrip(r.WithContext(ctx))
	if err != nil {
		if he := stalled(err); he != nil {
			return nil, &Error{Reason: ReasonTimeout, Err: he}
		}
	}
	return resp, err
}

// capBody limits every response body to MaxResponse bytes, so a peer cannot
// exhaust memory through a decoder that reads to the end.
type capBody struct{ http.RoundTripper }

func (c capBody) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.RoundTripper.RoundTrip(r)
	if err == nil {
		resp.Body = &cappedReader{rc: resp.Body, left: MaxResponse}
	}
	return resp, err
}

type cappedReader struct {
	rc   io.ReadCloser
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.rc.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, errTooLarge
	}
	return n, err
}

func (c *cappedReader) Close() error { return c.rc.Close() }

// pinnedTLSConfig is the one outbound TLS configuration: h's certificate,
// TLS 1.3 only, and acceptance of exactly p's fingerprint. onMismatch, when
// set, runs before a pin failure is returned.
func pinnedTLSConfig(h Host, p Peer, onMismatch func()) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{h.Certificate},
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || Fingerprint(cs.PeerCertificates[0]) != p.Fingerprint {
				if onMismatch != nil {
					onMismatch()
				}
				return errPinMismatch
			}
			return nil
		},
	}
}

// Do sends one request to the peer's gateway and returns the status and a
// body of at most MaxResponse bytes. The error is a *Error.
func (c *Client) Do(ctx context.Context, method, path string, hdr http.Header, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.peer.Address+path, rd)
	if err != nil {
		return 0, nil, &Error{Reason: ReasonUnreachable, Err: err}
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	hc, tr := c.httpClient()
	defer tr.CloseIdleConnections()
	ctx, stalled := traceHandshake(ctx, c.peer.Address)
	req = req.WithContext(ctx)
	resp, err := hc.Do(req)
	if err != nil {
		if he := stalled(err); he != nil {
			return 0, nil, &Error{Reason: ReasonTimeout, Err: he}
		}
		return 0, nil, &Error{Reason: Classify(err), Err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return 0, nil, &Error{Reason: Classify(err), Err: err}
	}
	if len(b) > MaxResponse {
		return 0, nil, &Error{Reason: ReasonUnreachable, Err: errTooLarge}
	}
	return resp.StatusCode, b, nil
}

// Agents fetches the peer's listing. A row that fails validation is dropped;
// a body that is unusable as a whole is invalid_listing.
func (c *Client) Agents(ctx context.Context) ([]AgentRow, error) {
	status, b, err := c.Do(ctx, http.MethodGet, agentsPath, nil, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusForbidden:
		return nil, &Error{Reason: ReasonUnpairedByPeer, Err: errors.New("403")}
	case status != http.StatusOK:
		return nil, &Error{Reason: ReasonUnreachable, Err: fmt.Errorf("status %d", status)}
	}
	var doc struct {
		Agents *[]json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.Agents == nil || len(*doc.Agents) > MaxRows {
		return nil, &Error{Reason: ReasonInvalidListing, Err: err}
	}
	rows := make([]AgentRow, 0, len(*doc.Agents))
	seen := map[string]bool{}
	for _, raw := range *doc.Agents {
		var r AgentRow
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		r, ok := cleanRow(r)
		if !ok || seen[r.AgentID] {
			continue
		}
		seen[r.AgentID] = true
		rows = append(rows, r)
	}
	if len(*doc.Agents) > 0 && len(rows) == 0 {
		return nil, &Error{Reason: ReasonInvalidListing}
	}
	return rows, nil
}

// Classify maps a transport error to a Reason* code.
func Classify(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, errPinMismatch):
		return ReasonPinMismatch
	case errors.Is(err, syscall.ECONNREFUSED):
		return ReasonGatewayNotRunning
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return ReasonTimeout
	case isAlert(err):
		return ReasonUnpairedByPeer
	}
	return ReasonUnreachable
}
