// Package a2a is the A2A ingress and client of a shim agent (design §5): an
// A2A 1.0 JSON-RPC server per agent that feeds the receiver chain, and the
// client shims use to send to each other.
package a2a

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

const (
	// SenderHeader names the registered agent whose own token the bearer
	// credential is; absent means the bearer is the peer token.
	SenderHeader = "X-CrossTalk-Agent"

	// Gateway-set headers: the gateway writes these on the loopback hop after
	// it stripped every inbound X-CrossTalk-* header; they are distinct from
	// the X-CrossTalk-Remote-* names a sending host puts on the wire.
	GWHostHeader    = "X-CrossTalk-GW-Host"
	GWAliasHeader   = "X-CrossTalk-GW-Alias"
	GWAgentIDHeader = "X-CrossTalk-GW-Agent-ID"
	GWNameHeader    = "X-CrossTalk-GW-Name"
	GWHarnessHeader = "X-CrossTalk-GW-Harness"
	GWRoleHeader    = "X-CrossTalk-GW-Role"
	SchemeBearer    = sdk.SecuritySchemeName("bearer")
	MetaKey         = "crosstalk"

	CompletionDelivered = "delivered"
	ReasonDeadline      = "deadline"
	ReasonTooManyWaits  = "too_many_waits"
	ReasonTooManyAsks   = "too_many_asks"

	DefaultMaxWaiting  = 64
	DefaultMaxOpenAsks = 32

	// MaxRequestBytes bounds one JSON-RPC request body, read before decode.
	MaxRequestBytes = 256 << 10
)

var (
	ErrNoSuchAsk  = errors.New("a2a: no ask task for message_id")
	ErrNoSuchTask = errors.New("a2a: no task for message_id")
	ErrAskClosed  = errors.New("a2a: ask task is already terminal")
	ErrNotListen  = errors.New("a2a: Listen has not been called")
	errTokenMode  = errors.New("a2a: token file is not a 0600 regular file")
)

type Receiver interface {
	Receive(ctx context.Context, in receiver.Inbound) receiver.Result
	Cancel(messageID string) (bool, error)
}

// Profile is what the Agent Card says about the served agent; it is read per
// card request because a rename or claim changes it.
type Profile struct {
	Name    string
	Harness string
	Role    string
}

// Peer is a registered agent that may authenticate with its own token.
type Peer struct {
	AgentID   string
	Role      string
	Harness   string
	TokenPath string
}

type Limits struct {
	MaxWaiting  int
	MaxOpenAsks int
	// MaxPending bounds unresolved (submitted or working) tasks.
	MaxPending      int
	MaxMessageBytes int
}

type Config struct {
	Receiver      Receiver
	Profile       func() Profile
	Version       string
	PeerTokenPath string
	// GatewayTokenPath is the gateway credential file; empty disables the
	// remote-sender case in authenticate.
	GatewayTokenPath string
	ResolvePeer      func(name string) (Peer, bool)
	// Claim, when set, serves metadata.crosstalk.claim requests; nil refuses
	// them as unsupported.
	Claim  ClaimFunc
	Limits Limits
	Logger *slog.Logger
	// ResolveSender rebuilds the sender of a replayed task from its
	// principal; nil, or false, falls back to the principal alone.
	ResolveSender func(principal string) (receiver.Sender, bool)
	// Tasks persists tasks; nil keeps them in memory for this Server only.
	Tasks TaskStore
	// OnAskClosed, when set, receives every message id of an ask task once
	// it is terminal (completed, failed, canceled), and of every terminal
	// ask task replayed by NewServer. It runs without the task lock held.
	OnAskClosed func(messageIDs []string)
}

type Server struct {
	cfg Config

	mu        sync.Mutex
	ln        net.Listener
	srv       *http.Server
	url       string
	peerHash  [32]byte
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
	drained   chan struct{}

	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	tasks   *store
	waiting int

	beforeReceive func(messageID string)
}

func NewServer(c Config) *Server {
	if c.Limits.MaxWaiting == 0 {
		c.Limits.MaxWaiting = DefaultMaxWaiting
	}
	if c.Limits.MaxOpenAsks == 0 {
		c.Limits.MaxOpenAsks = DefaultMaxOpenAsks
	}
	if c.Limits.MaxPending == 0 {
		c.Limits.MaxPending = receiver.DefaultLimits.MaxPending
	}
	if c.Limits.MaxMessageBytes == 0 {
		c.Limits.MaxMessageBytes = receiver.DefaultLimits.MaxMessageBytes
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Tasks == nil {
		c.Tasks = NewMemoryTaskStore()
	}
	base, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: c, base: base, cancel: cancel, closed: make(chan struct{}), drained: make(chan struct{}), tasks: newStore(c.Tasks, c.Logger)}
	for _, id := range s.tasks.pending() {
		if t, ok := s.tasks.byMessage(id); ok {
			s.armDeadline(t)
		}
	}
	for _, t := range s.tasks.terminalAsks() {
		s.askClosed(t)
	}
	return s
}

// Resumer is a Receiver that can re-run the chain for a message accepted
// before a restart without treating its own dedupe record as a duplicate.
type Resumer interface {
	Resume(ctx context.Context, in receiver.Inbound) receiver.Result
}

// Resume restarts every replayed task still submitted: its journaled message
// goes through the receiver again, which re-runs policy and dispatch only
// when no dispatch claim exists. A task whose message cannot be rebuilt is
// resolved unknown with reason journal. Call it once, after journal recovery
// and reconcile, so submitted work never stays silently inaccessible.
func (s *Server) Resume() {
	if s.cfg.Receiver == nil {
		return
	}
	for _, id := range s.tasks.pending() {
		t, ok := s.tasks.byMessage(id)
		if !ok {
			continue
		}
		in, ok := s.tasks.resumeInbound(t, s.cfg.ResolveSender)
		if !ok {
			s.apply(t, receiver.Result{MessageID: id, State: adapter.Unknown, Reason: receiver.ReasonJournal})
			continue
		}
		if in == nil {
			continue
		}
		if !s.start(t, *in, true) {
			return
		}
	}
}

// Listen reads (creating it on first start) the peer token and binds
// 127.0.0.1:0. It returns the server URL, which is both the Agent Card base
// URL and the JSON-RPC endpoint.
func (s *Server) Listen() (string, error) {
	tok, err := udsingress.EnsureKey(s.cfg.PeerTokenPath)
	if err != nil {
		return "", fmt.Errorf("a2a: peer token: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	url := "http://" + ln.Addr().String()

	h := &a2asrv.InterceptedHandler{
		Handler:      &handler{s: s},
		Interceptors: []a2asrv.CallInterceptor{&authInterceptor{s: s}, s.enforcer()},
		Logger:       s.cfg.Logger,
	}
	mux := http.NewServeMux()
	mux.Handle("/{$}", s.admitted(limitBody(a2asrv.NewJSONRPCHandler(h))))
	mux.Handle(a2asrv.WellKnownAgentCardPath, loopbackCard(a2asrv.NewAgentCardHandler(cardProducer{s: s})))

	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerHash = sha256.Sum256([]byte(tok))
	s.ln = ln
	s.url = url
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(s.cfg.Logger.Handler(), slog.LevelWarn)}
	return url, nil
}

func loopbackHost(hostport string) bool {
	h := hostport
	if sh, _, err := net.SplitHostPort(hostport); err == nil {
		h = sh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return h == "127.0.0.1" || h == "::1" || strings.EqualFold(h, "localhost")
}

// loopbackCard serves the card to loopback Host values only and removes every
// CORS header the SDK handler sets (ADR 0011).
func loopbackCard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(&corsStripper{ResponseWriter: w}, r)
	})
}

type corsStripper struct{ http.ResponseWriter }

func (c *corsStripper) strip() {
	h := c.Header()
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") || strings.EqualFold(k, "Vary") {
			delete(h, k)
		}
	}
}

func (c *corsStripper) WriteHeader(code int) {
	c.strip()
	c.ResponseWriter.WriteHeader(code)
}

func (c *corsStripper) Write(b []byte) (int, error) {
	c.strip()
	return c.ResponseWriter.Write(b)
}

// admitted holds the shutdown drain for the whole request: http.Server.Close
// does not wait for running handlers, and they write the journal.
func (s *Server) admitted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.admit() {
			http.Error(w, "server closing", http.StatusServiceUnavailable)
			return
		}
		defer s.wg.Done()
		next.ServeHTTP(w, r)
	})
}

// limitBody rejects oversized requests as JSON-RPC invalid-request errors so the
// sender sees a definite rejection rather than an ambiguous HTTP status.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxRequestBytes {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"request body exceeds %d bytes"}}`+"\n", MaxRequestBytes)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Serve serves until ctx is done or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	srv, ln := s.srv, s.ln
	s.mu.Unlock()
	if srv == nil {
		return ErrNotListen
	}
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ErrWorkersRunning: Close gave up waiting for in-flight receives, which may
// still dispatch.
var ErrWorkersRunning = errors.New("a2a: receives still running after close")

// Close drops open connections, including default-wait SendMessage calls,
// cancels in-flight receives and waits for them to return.
func (s *Server) Close() error { return s.CloseContext(context.Background()) }

// CloseContext is Close with the wait for in-flight receives bounded by ctx;
// on expiry it returns ErrWorkersRunning.
func (s *Server) CloseContext(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		close(s.closed)
		srv, ln := s.srv, s.ln
		s.mu.Unlock()
		s.cancel()
		switch {
		case srv != nil:
			s.closeErr = srv.Close()
		case ln != nil:
			s.closeErr = ln.Close()
		}
		s.tasks.stopTimers()
		go func() {
			s.wg.Wait()
			close(s.drained)
		}()
	})
	select {
	case <-s.drained:
		return s.closeErr
	case <-ctx.Done():
		return errors.Join(s.closeErr, fmt.Errorf("%w: %w", ErrWorkersRunning, ctx.Err()))
	}
}

// CompleteAsk completes the ask task for messageID with replyText as its
// final message (§8 reply).
func (s *Server) CompleteAsk(messageID, replyText string) error {
	t, err := s.tasks.completeAsk(messageID, replyText)
	if err != nil {
		return err
	}
	s.askClosed(t)
	return nil
}

// AskOpen reports whether messageID is a handle (first message or a
// continuation) of an ask task (known), the task's id, and whether the task
// is still open; a reply to a known, closed ask must not be sent.
func (s *Server) AskOpen(messageID string) (taskID string, open, known bool) {
	return s.tasks.askOpen(messageID)
}

// AskMessageID returns the first message id of the ask task that handle (its
// first message or a continuation) belongs to.
func (s *Server) AskMessageID(handle string) (string, bool) {
	t, ok := s.tasks.byMessage(handle)
	if !ok || t.kind != msg.Ask {
		return "", false
	}
	return t.messageID, true
}

// Resolve applies a delivery outcome learned after the receive returned (a
// replay or a reconcile) to messageID's task: accepted or queued completes an
// inform or command, rejected or unreachable fails it, unknown leaves it
// working. A terminal task is left as it is.
func (s *Server) Resolve(messageID string, state adapter.State, reason string) error {
	t, ok := s.tasks.byMessage(messageID)
	if !ok {
		return ErrNoSuchTask
	}
	s.apply(t, receiver.Result{MessageID: messageID, State: state, Reason: reason})
	return nil
}

// cancel records a cancel in the namespace of the task's sender: a remote
// principal's message_id is not a local one.
func (s *Server) cancelTask(principal, messageID string) (bool, error) {
	if rc, ok := s.cfg.Receiver.(interface {
		CancelFrom(senderKey, messageID string) (bool, error)
	}); ok && receiver.IsRemoteKey(principal) {
		return rc.CancelFrom(principal, messageID)
	}
	return s.cfg.Receiver.Cancel(messageID)
}

// PendingMessageIDs lists the message_ids of non-terminal tasks, the ones a
// caller may reconcile with Resolve.
func (s *Server) PendingMessageIDs() []string { return s.tasks.pending() }

// PendingMessage is a non-terminal task's message_id with the journal scope of
// its dispatch claim (the sender's host-scoped key for a remote sender,
// otherwise empty).
type PendingMessage struct{ Scope, MessageID string }

// PendingMessages is PendingMessageIDs with each task's dispatch scope.
func (s *Server) PendingMessages() []PendingMessage { return s.tasks.pendingScoped() }

func (s *Server) enforcer() a2asrv.CallInterceptor {
	ic, err := a2asrv.NewAgentCardEnforcer(s.Card())
	if err != nil {
		panic(fmt.Sprintf("a2a: card enforcer: %v", err))
	}
	return ic
}

type senderKey struct{}

func senderFrom(ctx context.Context) (receiver.Sender, bool) {
	snd, ok := ctx.Value(senderKey{}).(receiver.Sender)
	return snd, ok
}

func principal(snd receiver.Sender) string {
	if snd.Host != "" {
		return receiver.RemoteKey(snd.Host, snd.AgentID)
	}
	if snd.AgentID != "" {
		return snd.AgentID
	}
	return snd.Principal
}

type authInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	s *Server
}

func (a *authInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	snd, err := a.s.authenticate(callCtx.ServiceParams())
	if err != nil {
		return ctx, nil, err
	}
	callCtx.User = a2asrv.NewAuthenticatedUser(principal(snd), nil)
	return context.WithValue(ctx, senderKey{}, snd), nil, nil
}

func (s *Server) authenticate(p *a2asrv.ServiceParams) (receiver.Sender, error) {
	auth, _ := p.Get("Authorization")
	if len(auth) != 1 {
		return receiver.Sender{}, sdk.ErrUnauthenticated
	}
	scheme, tok, ok := strings.Cut(auth[0], " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return receiver.Sender{}, sdk.ErrUnauthenticated
	}
	got := sha256.Sum256([]byte(tok))

	names, _ := p.Get(SenderHeader)
	if len(names) == 0 {
		s.mu.Lock()
		want := s.peerHash
		s.mu.Unlock()
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return receiver.Sender{Verified: true, Principal: receiver.PrincipalLocalUser}, nil
		}
		if s.gatewayCredential(got) {
			return remoteSender(p)
		}
		return receiver.Sender{}, sdk.ErrUnauthenticated
	}
	if len(names) == 1 && names[0] != "" && s.cfg.ResolvePeer != nil {
		peer, ok := s.cfg.ResolvePeer(names[0])
		if !ok || peer.AgentID == "" {
			return receiver.Sender{}, sdk.ErrUnauthenticated
		}
		own, err := readToken(peer.TokenPath)
		if err != nil {
			return receiver.Sender{}, sdk.ErrUnauthenticated
		}
		want := sha256.Sum256([]byte(own))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return receiver.Sender{}, sdk.ErrUnauthenticated
		}
		return receiver.Sender{Verified: true, AgentID: peer.AgentID, Name: names[0], Role: peer.Role, Harness: peer.Harness}, nil
	}
	return receiver.Sender{}, sdk.ErrUnauthenticated
}

// gatewayCredential reports whether the bearer hash is the gateway token. The
// file is read on every call (mode-checked), so a rotated token takes effect
// without a restart; a missing or loose file matches nothing.
func (s *Server) gatewayCredential(got [32]byte) bool {
	if s.cfg.GatewayTokenPath == "" {
		return false
	}
	tok, err := readToken(s.cfg.GatewayTokenPath)
	if err != nil {
		return false
	}
	want := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

var (
	hostFingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	agentIDRE         = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

const maxRemoteNameBytes = 256

// ReasonRemoteSender is the rejection reason for gateway headers that are
// missing, repeated or malformed.
const ReasonRemoteSender = "invalid_remote_sender"

// remoteSender builds the sender of a request that carried the gateway
// credential from the gateway-set headers. Each header must appear exactly
// once; a malformed value is a JSON-RPC rejection, never an HTTP error, so the
// far side reads it as rejected and not as unknown.
func remoteSender(p *a2asrv.ServiceParams) (receiver.Sender, error) {
	bad := func() (receiver.Sender, error) { return receiver.Sender{}, rejection("", ReasonRemoteSender) }
	one := func(k string) (string, bool) {
		v, _ := p.Get(k)
		if len(v) != 1 || v[0] == "" {
			return "", false
		}
		return v[0], true
	}
	host, ok := one(GWHostHeader)
	if !ok || !hostFingerprintRE.MatchString(host) {
		return bad()
	}
	alias, ok := one(GWAliasHeader)
	if !ok || !validAlias(alias) {
		return bad()
	}
	agentID, ok := one(GWAgentIDHeader)
	if !ok {
		return bad()
	}
	enc, ok := one(GWNameHeader)
	if !ok {
		return bad()
	}
	harness, ok := one(GWHarnessHeader)
	if !ok {
		return bad()
	}
	role, ok := one(GWRoleHeader)
	if !ok {
		return bad()
	}
	name, ok := ValidateAttested(agentID, enc, harness, role)
	if !ok {
		return bad()
	}
	return receiver.Sender{
		Verified: true, Host: host, HostAlias: alias, AgentID: agentID,
		Name: name + "@" + alias, Role: role, Harness: harness,
		Principal: receiver.RemoteKey(host, agentID),
	}, nil
}

// ValidateAttested checks the four fields a sending host attests about its
// agent: agent_id (16 lowercase hex), name (base64url, 1 to 256 bytes of UTF-8
// without control characters), harness and role. The gateway applies it to the
// X-CrossTalk-Remote-* request headers and remoteSender to the X-CrossTalk-GW-*
// headers the gateway rebuilds, so the two cannot disagree. It returns the
// decoded name.
func ValidateAttested(agentID, encName, harness, role string) (string, bool) {
	if !agentIDRE.MatchString(agentID) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encName)
	if err != nil || len(raw) == 0 || len(raw) > maxRemoteNameBytes || !utf8.Valid(raw) || strings.IndexFunc(string(raw), func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return "", false
	}
	if harness != "claude" && harness != "codex" {
		return "", false
	}
	if role != registry.RolePrimary && role != registry.RoleSubagent {
		return "", false
	}
	return string(raw), true
}

func validAlias(a string) bool {
	if a == "" || len(a) > 63 || !utf8.ValidString(a) || strings.ContainsRune(a, '@') {
		return false
	}
	return strings.IndexFunc(a, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func readToken(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return "", errTokenMode
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errTokenMode
	}
	return t, nil
}

// Card is the Agent Card (§5). It never carries a credential.
func (s *Server) Card() *sdk.AgentCard {
	var p Profile
	if s.cfg.Profile != nil {
		p = s.cfg.Profile()
	}
	text := []string{"text/plain"}
	return &sdk.AgentCard{
		Name:                p.Name,
		Description:         fmt.Sprintf("%s session, role %s. Task completion is delivery evidence, not model-read or work-done.", p.Harness, p.Role),
		Version:             s.cfg.Version,
		SupportedInterfaces: []*sdk.AgentInterface{sdk.NewAgentInterface(s.URL(), sdk.TransportProtocolJSONRPC)},
		DefaultInputModes:   text,
		DefaultOutputModes:  text,
		Capabilities:        sdk.AgentCapabilities{},
		SecuritySchemes: sdk.NamedSecuritySchemes{
			SchemeBearer: sdk.HTTPAuthSecurityScheme{Scheme: "Bearer", Description: "CrossTalk peer token (principal local-user) or a registered agent's own token with the " + SenderHeader + " header."},
		},
		SecurityRequirements: sdk.SecurityRequirementsOptions{{SchemeBearer: sdk.SecuritySchemeScopes{}}},
		Skills: []sdk.AgentSkill{
			{ID: "inform", Name: "inform", Description: "Deliver a message into the session. Completed means the harness accepted it, not that the model read or acted on it.", Tags: []string{"message", "inform"}, Examples: []string{"FYI: the build on main is green again."}},
			{ID: "command", Name: "command", Description: "Ask the session to do work within its existing permissions. Requires a registered agent allowed by the role/owner policy; set metadata.crosstalk.kind to \"command\".", Tags: []string{"message", "command"}, Examples: []string{"Run the unit tests and report failures."}},
			{ID: "ask", Name: "ask", Description: "Deliver a question; the task completes with the session's reply as its final message. Set metadata.crosstalk.kind to \"ask\".", Tags: []string{"message", "ask", "reply"}, Examples: []string{"Which branch are you working on?"}},
		},
	}
}

type cardProducer struct{ s *Server }

func (c cardProducer) Card(context.Context) (*sdk.AgentCard, error) { return c.s.Card(), nil }

// CardJSON adds metadata.crosstalk, which the SDK card type has no field for.
func (c cardProducer) CardJSON(context.Context) ([]byte, error) {
	b, err := json.Marshal(c.s.Card())
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	var p Profile
	if c.s.cfg.Profile != nil {
		p = c.s.cfg.Profile()
	}
	m["metadata"] = map[string]any{MetaKey: map[string]any{"role": p.Role, "harness": p.Harness}}
	return json.Marshal(m)
}

type handler struct{ s *Server }

var _ a2asrv.RequestHandler = (*handler)(nil)

func (h *handler) SendStreamingMessage(context.Context, *sdk.SendMessageRequest) iter.Seq2[sdk.Event, error] {
	return func(yield func(sdk.Event, error) bool) { yield(nil, sdk.ErrUnsupportedOperation) }
}

func (h *handler) SubscribeToTask(context.Context, *sdk.SubscribeToTaskRequest) iter.Seq2[sdk.Event, error] {
	return func(yield func(sdk.Event, error) bool) { yield(nil, sdk.ErrUnsupportedOperation) }
}

func (h *handler) GetTaskPushConfig(context.Context, *sdk.GetTaskPushConfigRequest) (*sdk.PushConfig, error) {
	return nil, sdk.ErrPushNotificationNotSupported
}

func (h *handler) ListTaskPushConfigs(context.Context, *sdk.ListTaskPushConfigRequest) (*sdk.ListTaskPushConfigResponse, error) {
	return nil, sdk.ErrPushNotificationNotSupported
}

func (h *handler) CreateTaskPushConfig(context.Context, *sdk.PushConfig) (*sdk.PushConfig, error) {
	return nil, sdk.ErrPushNotificationNotSupported
}

func (h *handler) DeleteTaskPushConfig(context.Context, *sdk.DeleteTaskPushConfigRequest) error {
	return sdk.ErrPushNotificationNotSupported
}

func (h *handler) GetExtendedAgentCard(context.Context, *sdk.GetExtendedAgentCardRequest) (*sdk.AgentCard, error) {
	return nil, sdk.ErrUnsupportedOperation
}
