// Package harness is the contract every agent harness implements (ADR 0008).
// internal/shim, internal/mcpserver, internal/a2a and internal/wire use only
// this package; cmd/crosstalk is the one importer of the concrete harness
// packages and lists them in trust order.
package harness

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// ErrAmbiguous is wrapped by Bind when the session is reachable through
// more than one harness endpoint: the returned Binding is still usable for
// sending, and inbound delivery is unreachable.
var ErrAmbiguous = errors.New("harness: session reachable through more than one endpoint")

type ClientInfo struct {
	Name    string
	Version string
}

// RequestContext is what the MCP server knows about one tools/call.
type RequestContext struct {
	ParentPID    int
	Meta         map[string]any
	Client       ClientInfo
	MCPSessionID string
}

// Session is one discovered harness session, with or without CrossTalk.
type Session struct {
	Harness    adapter.Harness
	SessionRef string
	Name       string
	Cwd        string
	Status     string
	Reachable  bool
	HarnessPID int
	Endpoint   string
}

// Self is the bound agent a harness renders or injects on behalf of.
type Self struct {
	Binding adapter.Binding
	UDSPath string
}

// Identity is the sender of an inbound message as the receiver verified it.
type Identity struct {
	Name     string
	Harness  string
	Address  string
	Verified bool
}

type Harness interface {
	adapter.Adapter
	Name() adapter.Harness
	// Identify names the session a call comes from without contacting the
	// harness; it runs on every call. ok=false means "not mine". An empty
	// Name means the caller is named from Discover.
	Identify(ctx context.Context, req RequestContext) (s Session, ok bool, err error)
	// Bind confirms the binding when the agent registers. An error wrapping
	// ErrAmbiguous comes with a usable Binding.
	Bind(ctx context.Context, req RequestContext) (b adapter.Binding, ok bool, err error)
	Discover(ctx context.Context) ([]Session, error)
	// Render produces what the harness's model sees for an inbound message.
	Render(m msg.Message, from Identity) any
	// NativeReplyAddress is what Inject is given as Delivery.ReplyAddress
	// when self injects into this harness, or "" when the harness has no
	// native reply path.
	NativeReplyAddress(self Self) string
}

// Noticer is implemented by a harness whose model is told, with every
// delivered peer message, how little authority that text has. Mailbox entries
// pulled with receive_message carry the same text.
type Noticer interface {
	Notice() string
}

// IngressConfig is what the shim gives a native ingress for one agent.
type IngressConfig struct {
	Path      string
	Directory interface {
		List() ([]registry.Descriptor, error)
	}
	Receiver interface {
		Receive(ctx context.Context, in receiver.Inbound) receiver.Result
	}
	StartTime func(pid int) (time.Time, error)
	PeerPID   func(*net.UnixConn) (int, error)
}

type Ingress interface {
	Listen() error
	Serve(ctx context.Context) error
	Close() error
	// Files are removed when the agent exits, after Serve returns.
	Files() []string
}

// NativeIngress is implemented by a harness whose native senders reach an
// agent over a socket at the agent's uds_path. The first harness in the
// list that implements it serves that path for every agent; a nil Ingress
// means none.
type NativeIngress interface {
	NewIngress(cfg IngressConfig) Ingress
}

// Supersession is implemented by a harness that can prove a session ended
// while its MCP connection lives on, as Claude Code's /clear does. A harness
// without that proof does not implement it, and its sessions end only with
// their shim. One process serving several live sessions is not such proof.
type Supersession interface {
	// Superseded reports that s no longer runs in the process s.HarnessPID.
	// It must be false when the harness cannot tell.
	Superseded(s Session) bool
}

// Superseded is the registry's test for a descriptor, answered by the harness
// that wrote it.
func Superseded(hs []Harness) func(registry.Descriptor) bool {
	return func(d registry.Descriptor) bool {
		if d.HarnessPID == nil {
			return false
		}
		sup, ok := Lookup(hs, adapter.Harness(d.Harness)).(Supersession)
		return ok && sup.Superseded(Session{Harness: adapter.Harness(d.Harness), SessionRef: d.SessionRef, HarnessPID: *d.HarnessPID})
	}
}

// Lookup returns the harness named name.
func Lookup(hs []Harness, name adapter.Harness) Harness {
	for _, h := range hs {
		if h.Name() == name {
			return h
		}
	}
	return nil
}

// Identify returns the first harness's claim on req, in list order.
func Identify(ctx context.Context, hs []Harness, req RequestContext) (Session, bool, error) {
	for _, h := range hs {
		s, ok, err := h.Identify(ctx, req)
		if err != nil || ok {
			return s, ok, err
		}
	}
	return Session{}, false, nil
}
