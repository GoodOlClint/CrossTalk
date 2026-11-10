package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
)

var (
	ErrBadSessionID    = errors.New("codex claim: session_id is not a thread id")
	ErrPooled          = errors.New("codex claim: shim parent is an app-server (pooled)")
	ErrParentChanged   = errors.New("codex claim: shim parent pid or start time changed")
	ErrNotDescendant   = errors.New("codex claim: sender does not descend from the shim's parent")
	ErrLockFree        = errors.New("codex claim: writer lock is not held")
	ErrParentNotOpener = errors.New("codex claim: shim's parent does not have the lock file open")
	ErrSecondOpener    = errors.New("codex claim: another process has the lock file open")
)

const (
	DefaultProofTries    = 3
	DefaultProofInterval = 150 * time.Millisecond
)

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// System is what the owner proof reads from the kernel.
type System interface {
	Info(pid int) (ownerproof.Proc, error)
	Args(pid int) (exe string, argv []string, err error)
	LockHeld(path string) (bool, error)
	Openers(path string) ([]int, error)
}

// Kernel is the real System.
type Kernel struct{}

func (Kernel) Info(pid int) (ownerproof.Proc, error)  { return ownerproof.Info(pid) }
func (Kernel) Args(pid int) (string, []string, error) { return ownerproof.Args(pid) }
func (Kernel) LockHeld(path string) (bool, error)     { return ownerproof.LockHeld(path) }
func (Kernel) Openers(path string) ([]int, error)     { return ownerproof.Openers(path) }

// Claim is what the SessionStart hook hands over. OwnerPID, OwnerStartMicros
// and ThreadEnv are the hook's own view, kept for diagnosis and never used
// to bind: the owner is the shim's parent and the origin is the transport.
type Claim struct {
	SessionID        string    `json:"session_id"`
	Source           string    `json:"source,omitempty"`
	Cwd              string    `json:"cwd,omitempty"`
	TranscriptPath   string    `json:"transcript_path,omitempty"`
	OwnerPID         int       `json:"owner_pid"`
	OwnerStartMicros int64     `json:"owner_start_us"`
	ThreadEnv        string    `json:"codex_thread_id,omitempty"`
	At               time.Time `json:"at"`
}

// Binding is one proven claim.
type Binding struct {
	Claim Claim
	Live  bool
}

type ClaimOptions struct {
	Sys System
	// Parent is the shim's parent as read when the shim started.
	Parent ownerproof.Proc
	// LockDir is <codex home>/thread-writer-locks.
	LockDir   string
	Registrar harness.SessionRegistrar
	// Tries and Interval bound the debounce of the lock and opener reads;
	// zero means the defaults. Sleep replaces time.Sleep.
	Tries    int
	Interval time.Duration
	Sleep    func(time.Duration)
	Logger   *slog.Logger
}

// Claims is one shim's table of proven claims, one per thread.
type Claims struct {
	opt ClaimOptions

	mu     sync.Mutex
	byID   map[string]*Binding
	regMu  sync.Mutex
	logger *slog.Logger
}

func NewClaims(opt ClaimOptions) *Claims {
	if opt.Sys == nil {
		opt.Sys = Kernel{}
	}
	if opt.Tries <= 0 {
		opt.Tries = DefaultProofTries
	}
	if opt.Interval <= 0 {
		opt.Interval = DefaultProofInterval
	}
	if opt.Sleep == nil {
		opt.Sleep = time.Sleep
	}
	c := &Claims{opt: opt, byID: map[string]*Binding{}, logger: opt.Logger}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}

func lockPath(dir, thread string) string { return filepath.Join(dir, thread+".lock") }

func pooled(argv []string) bool { return slices.Contains(argv, "app-server") }

// parentNow fails when the shim's parent is gone, was replaced under the
// same pid, or is a pooled app-server.
func (c *Claims) parentNow() error {
	p, err := c.opt.Sys.Info(c.opt.Parent.PID)
	if errors.Is(err, ownerproof.ErrNoProcess) || (err == nil && !p.Same(c.opt.Parent)) {
		return ErrParentChanged
	}
	if err != nil {
		return err
	}
	_, argv, err := c.opt.Sys.Args(c.opt.Parent.PID)
	if err != nil {
		return err
	}
	if pooled(argv) {
		return ErrPooled
	}
	return nil
}

// ownsLock is the inference of plan 3.3: the kernel names no flock holder,
// so the lock is held by someone, the parent has the file open, and no other
// process does.
func (c *Claims) ownsLock(thread string) error {
	path := lockPath(c.opt.LockDir, thread)
	held, err := c.opt.Sys.LockHeld(path)
	if err != nil {
		return err
	}
	if !held {
		return ErrLockFree
	}
	openers, err := c.opt.Sys.Openers(path)
	if err != nil {
		return err
	}
	if !slices.Contains(openers, c.opt.Parent.PID) {
		return ErrParentNotOpener
	}
	if len(openers) > 1 {
		return ErrSecondOpener
	}
	return nil
}

// proveTarget is the target obligation. A failing lock or opener read is
// retried up to Tries times, Interval apart, so a transient second opener
// does not fail it; one that stays does.
func (c *Claims) proveTarget(thread string) error {
	if !sessionIDRe.MatchString(thread) {
		return ErrBadSessionID
	}
	if err := c.parentNow(); err != nil {
		return err
	}
	var err error
	for i := range c.opt.Tries {
		if i > 0 {
			c.opt.Sleep(c.opt.Interval)
		}
		if err = c.ownsLock(thread); err == nil {
			return nil
		}
	}
	return err
}

// AcceptLive takes a claim whose sender pid came from the socket's peer
// credential. The sender's kernel ancestry must reach the shim's parent.
func (c *Claims) AcceptLive(ctx context.Context, senderPID int, cl Claim) error {
	chain, err := ownerproof.AncestryWith(c.opt.Sys.Info, senderPID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotDescendant, err)
	}
	if !slices.ContainsFunc(chain, c.opt.Parent.Same) {
		return ErrNotDescendant
	}
	return c.accept(ctx, cl, true)
}

// AcceptDurable takes a claim read from the claim directory: only the target
// obligation can be checked.
func (c *Claims) AcceptDurable(ctx context.Context, cl Claim) error {
	return c.accept(ctx, cl, false)
}

func (c *Claims) accept(ctx context.Context, cl Claim, live bool) error {
	if err := c.proveTarget(cl.SessionID); err != nil {
		return err
	}
	c.regMu.Lock()
	defer c.regMu.Unlock()
	req := harness.RequestContext{Meta: map[string]any{"threadId": cl.SessionID}}
	if err := c.opt.Registrar.RegisterSession(ctx, req, cl.Cwd); err != nil {
		return err
	}
	c.mu.Lock()
	c.byID[cl.SessionID] = &Binding{Claim: cl, Live: live}
	c.mu.Unlock()
	return nil
}

func (c *Claims) Binding(thread string) (Binding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.byID[thread]
	if !ok {
		return Binding{}, false
	}
	return *b, true
}

func (c *Claims) Bound(thread string) bool { _, ok := c.Binding(thread); return ok }

// Threads lists the claimed threads, sorted.
func (c *Claims) Threads() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byID))
	for t := range c.byID {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// Recheck re-runs each claimed thread's own proof and revokes only the
// claims whose proof fails; a changed parent fails every proof. It returns
// the revoked threads, sorted.
func (c *Claims) Recheck() []string {
	c.mu.Lock()
	snap := make(map[string]*Binding, len(c.byID))
	for t, b := range c.byID {
		snap[t] = b
	}
	c.mu.Unlock()
	var revoked []string
	for t, b := range snap {
		err := c.proveTarget(t)
		if err == nil {
			continue
		}
		c.mu.Lock()
		if c.byID[t] == b {
			delete(c.byID, t)
			revoked = append(revoked, t)
		}
		c.mu.Unlock()
		c.logger.Info("codex claim revoked", "thread", t, "err", err)
	}
	slices.Sort(revoked)
	return revoked
}
