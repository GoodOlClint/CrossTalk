package claude

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type HarnessOptions struct {
	// SessionsDir is ~/.claude/sessions; "" means DefaultSessionsDir().
	SessionsDir string
	// Alive reports whether a session pid is running; nil means its process
	// start time can be read.
	Alive func(pid int) bool
	// Ingress serves Claude's uds protocol (§5) on each agent's uds_path;
	// cmd/crosstalk passes udsingress.NewIngress, which cannot be imported
	// here because its tests import this package. nil means no ingress.
	Ingress func(harness.IngressConfig) harness.Ingress
	// PPID is the Claude Code process whose session ServeClaims registers;
	// 0 means os.Getppid().
	PPID int
	// PollInterval is how often ServeClaims reads that session file; 0
	// means DefaultPollInterval.
	PollInterval time.Duration
	// Directory is the live descriptor set ServeClaims checks before each
	// registration; nil means it registers on every poll.
	Directory interface {
		List() ([]registry.Descriptor, error)
	}
	// StateDir is the state directory holding the journals; "" means a
	// renamed session's pin is not read.
	StateDir string
}

const DefaultPollInterval = 2 * time.Second

// Harness is Claude Code behind harness.Harness: the caller is the session
// whose record names the parent pid, and the reply address is the injecting
// agent's uds path.
type Harness struct {
	a        Adapter
	ingress  func(harness.IngressConfig) harness.Ingress
	ppid     int
	interval time.Duration
	dir      interface {
		List() ([]registry.Descriptor, error)
	}
	stateDir string
	pin      pinCache
}

// pinCache holds the pin read from one journal file, re-read only when the
// file's size or modification time changes.
type pinCache struct {
	path string
	size int64
	mod  time.Time
	name string
}

var (
	_ harness.Harness       = (*Harness)(nil)
	_ harness.NativeIngress = (*Harness)(nil)
	_ harness.Supersession  = (*Harness)(nil)
	_ harness.ClaimSource   = (*Harness)(nil)
)

func NewHarness(opt HarnessOptions) (*Harness, error) {
	if opt.SessionsDir == "" {
		d, err := DefaultSessionsDir()
		if err != nil {
			return nil, err
		}
		opt.SessionsDir = d
	}
	if opt.PPID == 0 {
		opt.PPID = os.Getppid()
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = DefaultPollInterval
	}
	return &Harness{a: Adapter{SessionsDir: opt.SessionsDir, Alive: opt.Alive}, ingress: opt.Ingress, ppid: opt.PPID, interval: opt.PollInterval, dir: opt.Directory, stateDir: opt.StateDir}, nil
}

// ServeClaims registers the session named in the parent's session file
// whenever no live descriptor holds it under its harness name: when the file
// first appears (it may not exist yet at shim start), after /clear or
// /resume, on a harness rename, and while a fallback or retired registration
// leaves it so. A failure is retried every poll and logged once per session,
// name and error.
func (h *Harness) ServeClaims(ctx context.Context, r harness.SessionRegistrar) error {
	tick := time.NewTicker(h.interval)
	defer tick.Stop()
	var failed string
	for {
		if s, err := h.a.readSession(h.ppid); err == nil && !h.held(s) {
			if err := r.RegisterSession(ctx, harness.RequestContext{ParentPID: h.ppid}, s.Cwd); err != nil {
				if f := s.SessionID + "\x00" + s.Name + "\x00" + err.Error(); f != failed && ctx.Err() == nil {
					failed = f
					slog.Warn("claude: registering session", "session", s.SessionID, "err", err)
				}
			} else {
				failed = ""
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// held reports that a live descriptor, this shim's or another's, holds s
// under its harness name or under the name its journal pins.
func (h *Harness) held(s Session) bool {
	if h.dir == nil {
		return false
	}
	ds, err := h.dir.List()
	if err != nil {
		return false
	}
	id := identity.AgentID(string(adapter.Claude), s.SessionID)
	for _, d := range ds {
		if d.AgentID == id {
			if p := h.pinnedName(id); p != "" {
				return d.Name == p
			}
			return s.Name == "" || d.Name == s.Name
		}
	}
	return false
}

// pinnedName is the name the session set with rename (ADR 0027), or "". A
// saved pin outranks the harness name: the session is held only under it.
func (h *Harness) pinnedName(agentID string) string {
	if h.stateDir == "" {
		return ""
	}
	path := journal.Path(h.stateDir, agentID)
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if c := h.pin; c.path != path || c.size != fi.Size() || !c.mod.Equal(fi.ModTime()) {
		h.pin = pinCache{path, fi.Size(), fi.ModTime(), journal.PinnedName(h.stateDir, agentID)}
	}
	return h.pin.name
}

func (h *Harness) Name() adapter.Harness { return adapter.Claude }

func (h *Harness) Register(ctx context.Context, sessionRef string) (adapter.Binding, error) {
	return h.a.Register(ctx, sessionRef)
}

func (h *Harness) Status(ctx context.Context, sessionRef string) (adapter.SessionStatus, error) {
	return h.a.Status(ctx, sessionRef)
}

// Inject sends d.ReplyAddress as the envelope's reply address.
func (h *Harness) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	a := h.a
	a.UDSPath = d.ReplyAddress
	return a.Inject(ctx, m, d)
}

func (h *Harness) Identify(_ context.Context, req harness.RequestContext) (harness.Session, bool, error) {
	if req.ParentPID <= 1 {
		return harness.Session{}, false, nil
	}
	b, err := os.ReadFile(filepath.Join(h.a.SessionsDir, strconv.Itoa(req.ParentPID)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return harness.Session{}, false, nil
	}
	if err != nil {
		return harness.Session{}, false, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return harness.Session{}, false, err
	}
	if s.SessionID == "" {
		return harness.Session{}, false, nil
	}
	return harness.Session{Harness: adapter.Claude, SessionRef: s.SessionID, Name: s.Name, Cwd: s.Cwd, Status: s.Status,
		HarnessPID: req.ParentPID, Endpoint: s.MessagingSocketPath}, true, nil
}

func (h *Harness) Bind(_ context.Context, req harness.RequestContext) (adapter.Binding, bool, error) {
	a := h.a
	ppid := req.ParentPID
	a.PPID = func() int { return ppid }
	s, err := a.BindFromParent()
	if err != nil {
		return adapter.Binding{}, true, err
	}
	return s.Binding(), true, nil
}

// Superseded reports that the session file of s's process names another
// session. A missing or unreadable file is no evidence: Claude Code rewrites
// the file in place.
func (h *Harness) Superseded(s harness.Session) bool {
	if s.HarnessPID <= 0 {
		return false
	}
	cur, err := h.a.readSession(s.HarnessPID)
	return err == nil && cur.SessionID != s.SessionRef
}

// Discover reads every live session file; they are never consulted for
// sender identity (§3).
func (h *Harness) Discover(context.Context) ([]harness.Session, error) {
	paths, _ := filepath.Glob(filepath.Join(h.a.SessionsDir, "*.json"))
	var out []harness.Session
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cs Session
		if json.Unmarshal(b, &cs) != nil || cs.SessionID == "" || filepath.Base(p) != strconv.Itoa(cs.PID)+".json" || cs.PID <= 0 || !h.a.alive(cs.PID) {
			continue
		}
		out = append(out, harness.Session{Harness: adapter.Claude, SessionRef: cs.SessionID, Name: cs.Name, Cwd: cs.Cwd, Status: cs.Status,
			Reachable: socketExists(cs.MessagingSocketPath), HarnessPID: cs.PID, Endpoint: cs.MessagingSocketPath})
	}
	return out, nil
}

func socketExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// Render is the envelope Inject writes, with from.Address as the uds path
// a native reply reaches.
func (h *Harness) Notice(c msg.NoticeClass, fromHarness string) string {
	return msg.PeerNotice(c, fromHarness)
}

func (h *Harness) Render(m msg.Message, from harness.Identity) any {
	addr, err := FromAddress(from.Address)
	if err != nil {
		addr = ""
	}
	return EnvelopeIn(addr, from.Name, from.Harness, "", "", false, from.Notice, m)
}

func (h *Harness) NativeReplyAddress(self harness.Self) string { return self.UDSPath }

// NewIngress returns the Ingress option's server for cfg, or nil.
func (h *Harness) NewIngress(cfg harness.IngressConfig) harness.Ingress {
	if h.ingress == nil {
		return nil
	}
	return h.ingress(cfg)
}
