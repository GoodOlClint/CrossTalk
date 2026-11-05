package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// Threads is the Codex adapter plus loaded-thread discovery; *Adapter is
// the production one.
type Threads interface {
	adapter.Adapter
	LoadedThreads(ctx context.Context, extraEndpoints ...string) []LoadedThread
}

type HarnessOptions struct {
	// Adapter nil means an *Adapter on the default control socket with
	// Logger and ClientInfo.
	Adapter    Threads
	Logger     *slog.Logger
	ClientInfo appserver.ClientInfo
	// Directory supplies the harness_endpoint of every Codex descriptor,
	// queried alongside the adapter's endpoints (§3 CQ10); nil means none.
	Directory interface {
		List() ([]registry.Descriptor, error)
	}
}

// Harness is Codex behind harness.Harness: the caller is _meta.threadId,
// the binding is the one endpoint that has the thread loaded.
type Harness struct {
	Threads
}

var _ harness.Harness = (*Harness)(nil)

func NewHarness(opt HarnessOptions) *Harness {
	t := opt.Adapter
	if t == nil {
		t = &Adapter{Logger: opt.Logger, ClientInfo: opt.ClientInfo}
	}
	if a, ok := t.(*Adapter); ok && opt.Directory != nil {
		a.Directory = opt.Directory
	}
	return &Harness{Threads: t}
}

func (h *Harness) Name() adapter.Harness { return adapter.Codex }

func threadID(req harness.RequestContext) string {
	tid, _ := req.Meta["threadId"].(string)
	return tid
}

// Identify claims a call carrying _meta.threadId; _meta.sessionId alone is
// never an identity (§3).
func (h *Harness) Identify(_ context.Context, req harness.RequestContext) (harness.Session, bool, error) {
	tid := threadID(req)
	if tid == "" {
		return harness.Session{}, false, nil
	}
	return harness.Session{Harness: adapter.Codex, SessionRef: tid}, true, nil
}

func (h *Harness) Bind(ctx context.Context, req harness.RequestContext) (adapter.Binding, bool, error) {
	tid := threadID(req)
	if tid == "" {
		return adapter.Binding{}, false, nil
	}
	b, err := h.Register(ctx, tid)
	if errors.Is(err, ErrAmbiguousEndpoint) {
		return adapter.Binding{Harness: adapter.Codex, SessionRef: tid}, true, fmt.Errorf("%w: %w", harness.ErrAmbiguous, err)
	}
	if err != nil {
		return adapter.Binding{}, true, err
	}
	return b, true, nil
}

// Discover lists the loaded threads on the adapter's endpoints plus every
// harness_endpoint a Codex descriptor records. An absent daemon yields
// nothing.
func (h *Harness) Discover(ctx context.Context) ([]harness.Session, error) {
	var out []harness.Session
	for _, t := range h.LoadedThreads(ctx) {
		out = append(out, harness.Session{Harness: adapter.Codex, SessionRef: t.ID, Name: t.Name, Cwd: t.Cwd, Status: t.Status,
			Reachable: t.Session.Reachable, Endpoint: t.Endpoint})
	}
	return out, nil
}

func (h *Harness) Notice() string { return Preamble }

func (h *Harness) Render(m msg.Message, from harness.Identity) any {
	return Render(m, from.Verified, from.Harness)
}

func (h *Harness) NativeReplyAddress(harness.Self) string { return "" }
