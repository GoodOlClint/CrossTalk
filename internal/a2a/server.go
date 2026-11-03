// Package a2a is the A2A ingress and client of a shim agent (design §5): an
// A2A 1.0 JSON-RPC server per agent that feeds the receiver chain, and the
// client shims use to send to each other.
package a2a

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

const (
	// SenderHeader names the registered agent whose own token the bearer
	// credential is; absent means the bearer is the peer token.
	SenderHeader = "X-CrossTalk-Agent"
	SchemeBearer = sdk.SecuritySchemeName("bearer")
	MetaKey      = "crosstalk"

	CompletionDelivered = "delivered"
	ReasonDeadline      = "deadline"
	ReasonTooManyWaits  = "too_many_waits"
	ReasonTooManyAsks   = "too_many_asks"

	DefaultMaxWaiting  = 64
	DefaultMaxOpenAsks = 32
)

var (
	ErrNoSuchAsk = errors.New("a2a: no ask task for message_id")
	ErrAskClosed = errors.New("a2a: ask task is already terminal")
	ErrNotListen = errors.New("a2a: Listen has not been called")
	errTokenMode = errors.New("a2a: token file is not a 0600 regular file")
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
	Cwd     string
}

// Peer is a registered agent that may authenticate with its own token.
type Peer struct {
	AgentID   string
	Role      string
	TokenPath string
}

type Limits struct {
	MaxWaiting  int
	MaxOpenAsks int
}

type Config struct {
	Receiver      Receiver
	Profile       func() Profile
	Version       string
	PeerTokenPath string
	ResolvePeer   func(name string) (Peer, bool)
	// Claim, when set, serves metadata.crosstalk.claim requests; nil refuses
	// them as unsupported.
	Claim  ClaimFunc
	Limits Limits
	Logger *slog.Logger
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
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	base, cancel := context.WithCancel(context.Background())
	return &Server{cfg: c, base: base, cancel: cancel, closed: make(chan struct{}), tasks: newStore()}
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
	mux.Handle("/{$}", a2asrv.NewJSONRPCHandler(h))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewAgentCardHandler(cardProducer{s: s}))

	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerHash = sha256.Sum256([]byte(tok))
	s.ln = ln
	s.url = url
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(s.cfg.Logger.Handler(), slog.LevelWarn)}
	return url, nil
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

// Close drops open connections, including default-wait SendMessage calls,
// cancels in-flight receives and waits for them to return.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		s.cancel()
		s.mu.Lock()
		srv, ln := s.srv, s.ln
		s.mu.Unlock()
		switch {
		case srv != nil:
			err = srv.Close()
		case ln != nil:
			err = ln.Close()
		}
		s.tasks.stopTimers()
		s.wg.Wait()
	})
	return err
}

// CompleteAsk completes the ask task for messageID with replyText as its
// final message (§8 reply).
func (s *Server) CompleteAsk(messageID, replyText string) error {
	return s.tasks.completeAsk(messageID, replyText)
}

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
	snd, ok := a.s.authenticate(callCtx.ServiceParams())
	if !ok {
		return ctx, nil, sdk.ErrUnauthenticated
	}
	callCtx.User = a2asrv.NewAuthenticatedUser(principal(snd), nil)
	return context.WithValue(ctx, senderKey{}, snd), nil, nil
}

func (s *Server) authenticate(p *a2asrv.ServiceParams) (receiver.Sender, bool) {
	auth, _ := p.Get("Authorization")
	if len(auth) != 1 {
		return receiver.Sender{}, false
	}
	scheme, tok, ok := strings.Cut(auth[0], " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return receiver.Sender{}, false
	}
	got := sha256.Sum256([]byte(tok))

	names, _ := p.Get(SenderHeader)
	switch {
	case len(names) == 0:
		s.mu.Lock()
		want := s.peerHash
		s.mu.Unlock()
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return receiver.Sender{}, false
		}
		return receiver.Sender{Verified: true, Principal: receiver.PrincipalLocalUser}, true
	case len(names) == 1 && names[0] != "" && s.cfg.ResolvePeer != nil:
		peer, ok := s.cfg.ResolvePeer(names[0])
		if !ok || peer.AgentID == "" {
			return receiver.Sender{}, false
		}
		own, err := readToken(peer.TokenPath)
		if err != nil {
			return receiver.Sender{}, false
		}
		want := sha256.Sum256([]byte(own))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return receiver.Sender{}, false
		}
		return receiver.Sender{Verified: true, AgentID: peer.AgentID, Name: names[0], Role: peer.Role}, true
	}
	return receiver.Sender{}, false
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
	m["metadata"] = map[string]any{MetaKey: map[string]any{"role": p.Role, "harness": p.Harness, "cwd": p.Cwd}}
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
