// Package wire connects shim agents over A2A (design §5, §8): each bound
// agent gets an a2a.Server, outbound sends and claims go through an
// a2a.Client, and a reply completes the ask's task.
package wire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/shim"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

const (
	ReasonPending  = "pending"
	ReasonNoA2AURL = "no_a2a_url"
	ReasonOwnToken = "own_token"

	// DefaultSettle bounds how long a returnImmediately send polls GetTask
	// for a delivery state past submitted (§8).
	DefaultSettle = 2 * time.Second
)

var errNoServer = errors.New("wire: no A2A server for agent")

// Wire is the A2A side of one shim process.
type Wire struct {
	shim     *shim.Shim
	stateDir string
	settle   time.Duration
	log      *slog.Logger

	mu      sync.Mutex
	servers map[string]*a2a.Server
}

var (
	_ shim.Transport      = (*Wire)(nil)
	_ shim.TaskTransport  = (*Wire)(nil)
	_ shim.ClaimTransport = (*Wire)(nil)
	_ shim.AskTransport   = (*Wire)(nil)
	_ shim.AskOrigin      = (*Wire)(nil)
)

// New builds the shim with opt's Transport, StartA2A and OnReply set to this
// Wire.
func New(opt shim.Options) (*Wire, error) {
	if opt.StateDir == "" {
		d, err := registry.DefaultDir()
		if err != nil {
			return nil, err
		}
		opt.StateDir = d
	}
	w := &Wire{stateDir: opt.StateDir, settle: DefaultSettle, log: opt.Logger, servers: map[string]*a2a.Server{}}
	if w.log == nil {
		w.log = slog.Default()
	}
	opt.Transport, opt.StartA2A, opt.OnReply = w, w.startA2A, w.onReply
	sh, err := shim.New(opt)
	if err != nil {
		return nil, err
	}
	w.shim = sh
	return w, nil
}

func (w *Wire) Shim() *shim.Shim { return w.shim }

// TokenPath is agentID's own bearer token, published as its credential_ref.
func (w *Wire) TokenPath(agentID string) string {
	return filepath.Join(w.stateDir, "tokens", agentID+".token")
}

// PeerTokenPath is the per-user peer token; a2a.Server.Listen creates it.
func (w *Wire) PeerTokenPath() string { return filepath.Join(w.stateDir, "peer.token") }

type agentReceiver struct{ st *shim.AgentState }

func (r agentReceiver) Receive(ctx context.Context, in receiver.Inbound) receiver.Result {
	return r.st.Receive(ctx, in)
}

func (r agentReceiver) Resume(ctx context.Context, in receiver.Inbound) receiver.Result {
	return r.st.Receiver().Resume(ctx, in)
}

func (r agentReceiver) Cancel(messageID string) (bool, error) {
	return r.st.Receiver().Cancel(messageID)
}

func (r agentReceiver) IDHeldByOther(senderKey, messageID string) bool {
	return r.st.Receiver().IDHeldByOther(senderKey, messageID)
}

func (r agentReceiver) CancelFrom(senderKey, messageID string) (bool, error) {
	return r.st.Receiver().CancelFrom(senderKey, messageID)
}

func (w *Wire) startA2A(_ context.Context, st *shim.AgentState) (shim.A2AListener, error) {
	a := st.Agent()
	id := a.ID()
	tok := w.TokenPath(id)
	if _, err := udsingress.EnsureKey(tok); err != nil {
		return shim.A2AListener{}, fmt.Errorf("wire: own token: %w", err)
	}
	tasks, err := a2a.NewJournalTaskStore(a.Journal(), journal.Options{})
	if err != nil {
		return shim.A2AListener{}, fmt.Errorf("wire: task replay: %w", err)
	}
	srv := a2a.NewServer(a2a.Config{
		Tasks:    tasks,
		Receiver: agentReceiver{st},
		Profile: func() a2a.Profile {
			d := a.Descriptor()
			role := d.Role
			if role == "" {
				role = registry.RolePrimary
			}
			return a2a.Profile{Name: a.Name(), Harness: string(a.Harness()), Role: role}
		},
		Version:          mcpserver.Version,
		PeerTokenPath:    w.PeerTokenPath(),
		GatewayTokenPath: filepath.Join(w.stateDir, "gateway.token"),
		ResolvePeer:      w.resolvePeer,
		ResolveSender:    w.resolveSender,
		Claim: func(ctx context.Context, snd receiver.Sender, claimed bool) error {
			return claimError(w.shim.ApplyClaim(ctx, registry.Descriptor{AgentID: snd.AgentID, Name: snd.Name}, id, claimed))
		},
		Logger: w.log.With("agent", id),
		OnAskClosed: func(ids []string) {
			for _, m := range ids {
				st.Receiver().CloseRoute(m)
			}
		},
	})
	reconcile(srv, a.Journal(), w.log.With("agent", id))
	srv.Resume()
	url, err := srv.Listen()
	if err != nil {
		srv.Close()
		return shim.A2AListener{}, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(context.Background()); err != nil {
			w.log.Warn("wire: a2a server stopped", "agent", id, "err", err)
		}
	}()
	w.mu.Lock()
	w.servers[id] = srv
	w.mu.Unlock()
	return shim.A2AListener{URL: url, CredentialRef: tok, Close: func(ctx context.Context) error {
		w.mu.Lock()
		if w.servers[id] == srv {
			delete(w.servers, id)
		}
		w.mu.Unlock()
		err := srv.CloseContext(ctx)
		select {
		case <-done:
		case <-ctx.Done():
		}
		return err
	}}, nil
}

// reconcile applies dispatch outcomes the journal holds for tasks still open
// after replay. It runs after receiver.Recover, which turns a claim with no
// outcome into unknown. A later outcome change has no receiver hook yet; the
// code that records one must call Server.Resolve for that message_id.
func reconcile(srv *a2a.Server, j *journal.Journal, log *slog.Logger) {
	for _, p := range srv.PendingMessages() {
		r, ok := j.DispatchStateFor(p.Scope, p.MessageID)
		if !ok || r.Type != journal.TypeOutcome {
			continue
		}
		if err := srv.Resolve(p.MessageID, adapter.State(r.State), ""); err != nil {
			log.Warn("wire: task reconcile", "message_id", p.MessageID, "err", err)
		}
	}
}

// resolvePeer accepts a sender's own token only at the path this package
// issues for its agent_id, whatever credential_ref a descriptor names.
func (w *Wire) resolvePeer(name string) (a2a.Peer, bool) {
	d, ok, err := w.shim.Registry().Lookup(name)
	if err != nil || !ok || d.AgentID == "" || d.CredentialRef != w.TokenPath(d.AgentID) {
		return a2a.Peer{}, false
	}
	role := d.Role
	if role == "" {
		role = registry.RolePrimary
	}
	return a2a.Peer{AgentID: d.AgentID, Role: role, Harness: d.Harness, TokenPath: d.CredentialRef}, true
}

func (w *Wire) resolveSender(agentID string) (receiver.Sender, bool) {
	ds, err := w.shim.Registry().List()
	if err != nil {
		return receiver.Sender{}, false
	}
	for _, d := range ds {
		if d.AgentID == agentID {
			role := d.Role
			if role == "" {
				role = registry.RolePrimary
			}
			return receiver.Sender{Verified: true, AgentID: agentID, Name: d.Name, Role: role, Harness: d.Harness}, true
		}
	}
	return receiver.Sender{}, false
}

func claimError(err error) error {
	var me *mcpserver.Error
	if errors.As(err, &me) {
		return &a2a.ClaimError{State: adapter.State(me.Code), Reason: me.Reason}
	}
	return err
}

func (w *Wire) onReply(_ context.Context, st *shim.AgentState, original, reply msg.Message) error {
	w.mu.Lock()
	srv := w.servers[st.Agent().ID()]
	w.mu.Unlock()
	if srv == nil {
		return errNoServer
	}
	var text strings.Builder
	for _, p := range reply.Parts {
		text.WriteString(p.Text)
	}
	err := srv.CompleteAsk(original.MessageID, text.String())
	switch {
	case errors.Is(err, a2a.ErrAskClosed):
		return shim.ErrAskClosed
	case err != nil && !errors.Is(err, a2a.ErrNoSuchAsk):
		return err
	}
	return nil
}

// AskOpen reports, for an agent this Wire serves, whether messageID is a
// handle of an ask task, the task's id, and whether it is still open.
func (w *Wire) AskOpen(agentID, messageID string) (taskID string, open, known bool) {
	w.mu.Lock()
	srv := w.servers[agentID]
	w.mu.Unlock()
	if srv == nil {
		return "", false, false
	}
	return srv.AskOpen(messageID)
}

// AskMessageID returns the original ask message id of the task that handle
// belongs to, for an agent this Wire serves.
func (w *Wire) AskMessageID(agentID, handle string) (string, bool) {
	w.mu.Lock()
	srv := w.servers[agentID]
	w.mu.Unlock()
	if srv == nil {
		return "", false
	}
	return srv.AskMessageID(handle)
}

func readOwnToken(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("token %s is not a 0600 regular file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("token %s is empty", path)
	}
	return t, nil
}

func (w *Wire) Send(ctx context.Context, from, to registry.Descriptor, m msg.Message, returnImmediately bool) (adapter.State, error) {
	_, state, err := w.SendTask(ctx, from, to, m, returnImmediately)
	return state, err
}

// SendTask posts m to to's A2A server with from's own token. A
// returnImmediately task is polled up to the settle bound for its delivery
// state; one still submitted or dispatching is unknown with reason pending.
func (w *Wire) SendTask(ctx context.Context, from, to registry.Descriptor, m msg.Message, returnImmediately bool) (string, adapter.State, error) {
	if to.A2AURL == "" {
		return "", adapter.Unreachable, errors.New(ReasonNoA2AURL)
	}
	tok, err := readOwnToken(from.CredentialRef)
	if err != nil {
		return "", adapter.Unreachable, fmt.Errorf("%s: %w", ReasonOwnToken, err)
	}
	c := &a2a.Client{Name: from.Name}
	id, state, err := c.Send(ctx, to.A2AURL, tok, m, returnImmediately)
	if err != nil {
		return id, state, errors.New(remoteReason(err))
	}
	if !settling(state) || id == "" {
		return id, state, nil
	}
	deadline := time.Now().Add(w.settle)
	wait := 10 * time.Millisecond
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return id, adapter.Unknown, errors.New(ReasonPending)
		case <-time.After(min(wait, time.Until(deadline))):
		}
		pctx, cancel := context.WithDeadline(ctx, deadline)
		t, err := c.GetTask(pctx, to.A2AURL, tok, id)
		cancel()
		if err == nil && !settling(t.State) {
			if t.Reason != "" && t.State != adapter.Accepted && t.State != adapter.Queued {
				return id, t.State, errors.New(t.Reason)
			}
			return id, t.State, nil
		}
		wait = min(wait*2, 200*time.Millisecond)
	}
	return id, adapter.Unknown, errors.New(ReasonPending)
}

func settling(s adapter.State) bool {
	return s == journal.StateSubmitted || s == journal.StateDispatching
}

func remoteReason(err error) string {
	var ae *sdk.Error
	if errors.As(err, &ae) {
		if ct, ok := ae.Details[a2a.MetaKey].(map[string]any); ok {
			if r, ok := ct["reason"].(string); ok && r != "" {
				return r
			}
		}
	}
	return err.Error()
}

// SetClaim sends owner's claim or release to target's A2A server (§7).
func (w *Wire) SetClaim(ctx context.Context, owner, target registry.Descriptor, claimed bool) error {
	if target.A2AURL == "" {
		return &mcpserver.Error{Code: string(adapter.Unreachable), Reason: ReasonNoA2AURL}
	}
	tok, err := readOwnToken(owner.CredentialRef)
	if err != nil {
		return &mcpserver.Error{Code: string(adapter.Unreachable), Reason: ReasonOwnToken}
	}
	err = (&a2a.Client{Name: owner.Name}).Claim(ctx, target.A2AURL, tok, claimed)
	var ce *a2a.ClaimError
	if errors.As(err, &ce) {
		return &mcpserver.Error{Code: string(ce.State), Reason: ce.Reason}
	}
	return err
}
