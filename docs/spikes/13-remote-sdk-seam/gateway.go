// Package sdkseam is throwaway spike code (docs/spikes/13-remote-sdk-seam.md).
package sdkseam

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

const (
	HdrHost      = "X-CrossTalk-Remote-Host"
	HdrAlias     = "X-CrossTalk-Remote-Alias"
	HdrAgentID   = "X-CrossTalk-Remote-Agent-ID"
	HdrName      = "X-CrossTalk-Remote-Name"
	HdrHarness   = "X-CrossTalk-Remote-Harness"
	HdrRole      = "X-CrossTalk-Remote-Role"
	MaxNameBytes = 256
)

type Host struct {
	Cert tls.Certificate
	FP   string
}

func NewHost() (Host, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Host{}, err
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "crosstalk-spike"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		return Host{}, err
	}
	sum := sha256.Sum256(der)
	return Host{Cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, FP: hex.EncodeToString(sum[:])}, nil
}

func fingerprint(cs *tls.ConnectionState) string {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		return ""
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

// Remote is what the gateway attests to the loopback server.
type Remote struct {
	Host, Alias, AgentID, Name, Harness, Role string
}

var (
	agentIDRE = regexp.MustCompile(`^[0-9a-f]{16}$`)
	hostRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// validate checks the four attested fields in h, which must each appear once.
func validate(h http.Header) (Remote, error) {
	one := func(k string) (string, error) {
		v := h.Values(k)
		if len(v) != 1 || v[0] == "" {
			return "", fmt.Errorf("%s: want exactly one non-empty value, got %d", k, len(v))
		}
		return v[0], nil
	}
	var r Remote
	var err error
	if r.AgentID, err = one(HdrAgentID); err != nil {
		return r, err
	}
	if !agentIDRE.MatchString(r.AgentID) {
		return r, errors.New("agent_id is not 16 lowercase hex")
	}
	enc, err := one(HdrName)
	if err != nil {
		return r, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return r, errors.New("name is not base64url")
	}
	if len(raw) > MaxNameBytes || !utf8.Valid(raw) || strings.IndexFunc(string(raw), unicode.IsControl) >= 0 {
		return r, errors.New("name too long, not UTF-8 or has control characters")
	}
	r.Name = string(raw)
	if r.Harness, err = one(HdrHarness); err != nil {
		return r, err
	}
	if r.Harness != "claude" && r.Harness != "codex" {
		return r, errors.New("harness not claude|codex")
	}
	if r.Role, err = one(HdrRole); err != nil {
		return r, err
	}
	if r.Role != "primary" && r.Role != "subagent" {
		return r, errors.New("role not primary|subagent")
	}
	return r, nil
}

// EncodeName is what a sending host does to a name before the header.
func EncodeName(n string) string { return base64.RawURLEncoding.EncodeToString([]byte(n)) }

type Gateway struct {
	Addr    string
	Hits    atomic.Int64 // requests that passed the pin and were forwarded
	ln      net.Listener
	srv     *http.Server
	mu      sync.Mutex
	pins    map[string]string // fingerprint -> alias, this host's own record
	token   string
	backend *url.URL
}

// NewGateway serves TLS 1.3 on 127.0.0.1:0 and forwards /v1/a2a/{agent_id} to backend.
func NewGateway(self Host, backend, token string, pins map[string]string) (*Gateway, error) {
	bu, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}
	g := &Gateway{pins: pins, token: token, backend: bu}
	cfg := &tls.Config{
		Certificates:           []tls.Certificate{self.Cert},
		MinVersion:             tls.VersionTLS13,
		ClientAuth:             tls.RequireAnyClientCert,
		SessionTicketsDisabled: true,
		InsecureSkipVerify:     true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if _, ok := g.alias(fingerprint(&cs)); !ok {
				return errors.New("client not pinned")
			}
			return nil
		},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		return nil, err
	}
	g.ln, g.Addr = ln, ln.Addr().String()
	rp := &httputil.ReverseProxy{Rewrite: g.rewrite}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/a2a/{agent}", func(w http.ResponseWriter, r *http.Request) {
		fp := fingerprint(r.TLS)
		if _, ok := g.alias(fp); !ok {
			http.Error(w, "unpaired", http.StatusForbidden)
			return
		}
		if !agentIDRE.MatchString(r.PathValue("agent")) {
			http.Error(w, "bad target", http.StatusBadRequest)
			return
		}
		rem, err := validate(r.Header)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = rem
		g.Hits.Add(1)
		rp.ServeHTTP(w, r)
	})
	g.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go g.srv.Serve(ln)
	return g, nil
}

func (g *Gateway) alias(fp string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.pins[fp]
	return a, ok
}

func (g *Gateway) Close() { g.srv.Close() }

func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	in := pr.In
	rem, _ := validate(in.Header)
	fp := fingerprint(in.TLS)
	alias, _ := g.alias(fp)
	pr.SetURL(g.backend)
	pr.Out.URL.Path, pr.Out.URL.RawPath = "/", ""
	h := pr.Out.Header
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-crosstalk-") {
			delete(h, k)
		}
	}
	h.Del("Authorization")
	h.Set("Authorization", "Bearer "+g.token)
	h.Set(HdrHost, fp)
	h.Set(HdrAlias, alias)
	h.Set(HdrAgentID, rem.AgentID)
	h.Set(HdrName, EncodeName(rem.Name))
	h.Set(HdrHarness, rem.Harness)
	h.Set(HdrRole, rem.Role)
}

// Backend is the loopback a2a-go v2.7.0 server feeding the real receiver.
type Backend struct {
	URL   string
	Hits  atomic.Int64 // requests that reached the loopback HTTP server at all
	Calls atomic.Int64 // requests that passed the interceptor and reached SendMessage
	Seen  func() []Remote
	srv   *http.Server
	token string
	mu    sync.Mutex
	seen  []Remote
}

type SenderShape int

const (
	// NamespacedPrincipal: Verified, AgentID empty, Principal "remote:<fp>/<agent_id>".
	NamespacedPrincipal SenderShape = iota
	// PlanShaped: AgentID and Role set, as plan section 3.3 reads, on today's Sender.
	PlanShaped
)

func ToSender(r Remote, shape SenderShape) receiver.Sender {
	p := "remote:" + r.Host + "/" + r.AgentID
	if shape == PlanShaped {
		return receiver.Sender{Verified: true, Principal: p, AgentID: r.AgentID, Name: r.Name + "@" + r.Alias, Role: r.Role, Harness: r.Harness}
	}
	return receiver.Sender{Verified: true, Principal: p, Name: r.Name + "@" + r.Alias, Role: r.Role, Harness: r.Harness}
}

type ctxKey struct{}

type auth struct {
	a2asrv.PassthroughCallInterceptor
	b *Backend
}

func (a *auth) Before(ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	p := cc.ServiceParams()
	get := func(k string) []string { v, _ := p.Get(k); return v }
	bearer := get("Authorization")
	if len(bearer) != 1 || subtle.ConstantTimeCompare([]byte(bearer[0]), []byte("Bearer "+a.b.token)) != 1 {
		return ctx, nil, sdk.ErrUnauthenticated
	}
	h := http.Header{}
	for _, k := range []string{HdrAgentID, HdrName, HdrHarness, HdrRole, HdrHost, HdrAlias} {
		for _, v := range get(k) {
			h.Add(k, v)
		}
	}
	rem, err := validate(h)
	if err != nil || !hostRE.MatchString(h.Get(HdrHost)) || len(h.Values(HdrHost)) != 1 {
		return ctx, nil, sdk.ErrUnauthenticated
	}
	rem.Host, rem.Alias = h.Get(HdrHost), h.Get(HdrAlias)
	a.b.mu.Lock()
	a.b.seen = append(a.b.seen, rem)
	a.b.mu.Unlock()
	cc.User = a2asrv.NewAuthenticatedUser("remote:"+rem.Host+"/"+rem.AgentID, nil)
	return context.WithValue(ctx, ctxKey{}, rem), nil, nil
}

type handler struct {
	a2asrv.RequestHandler
	b     *Backend
	rcv   *receiver.Receiver
	shape SenderShape
}

func (h *handler) SendMessage(ctx context.Context, req *sdk.SendMessageRequest) (sdk.SendMessageResult, error) {
	h.b.Calls.Add(1)
	rem := ctx.Value(ctxKey{}).(Remote)
	kind := msg.Inform
	if ct, ok := req.Message.Metadata["crosstalk"].(map[string]any); ok {
		if k, ok := ct["kind"].(string); ok {
			kind = msg.Kind(k)
		}
	}
	var text string
	for _, p := range req.Message.Parts {
		text += p.Text()
	}
	m := msg.Message{MessageID: req.Message.ID, ContextID: "ctx", To: "bob", Kind: kind, Delivery: msg.Immediate, SentAt: time.Now(), Parts: []msg.Part{{Type: "text", Text: text}}}
	res := h.rcv.Receive(ctx, receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: ToSender(rem, h.shape)})
	st := sdk.TaskStateCompleted
	if res.State == adapter.Rejected {
		st = sdk.TaskStateFailed
	}
	return &sdk.Task{ID: sdk.NewTaskID(), ContextID: "ctx", Status: sdk.TaskStatus{State: st},
		Metadata: map[string]any{"crosstalk": map[string]any{"state": string(res.State), "reason": res.Reason, "duplicate": res.Duplicate}}}, nil
}

func NewBackend(rcv *receiver.Receiver, token string, shape SenderShape) (*Backend, error) {
	b := &Backend{token: token}
	b.Seen = func() []Remote {
		b.mu.Lock()
		defer b.mu.Unlock()
		return append([]Remote(nil), b.seen...)
	}
	ih := &a2asrv.InterceptedHandler{Handler: &handler{b: b, rcv: rcv, shape: shape}, Interceptors: []a2asrv.CallInterceptor{&auth{b: b}}}
	rpc := a2asrv.NewJSONRPCHandler(ih)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b.URL = "http://" + ln.Addr().String()
	b.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b.Hits.Add(1); rpc.ServeHTTP(w, r) })}
	go b.srv.Serve(ln)
	return b, nil
}

func (b *Backend) Close() { b.srv.Close() }
