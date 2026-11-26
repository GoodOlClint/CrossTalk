package pi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// TokenEnv carries the per-registration token the extension passes to the
// shim it registers (ADR 0036).
const TokenEnv = "CROSSTALK_PI_TOKEN"

type Options struct {
	// PPID is the Pi process this shim serves; 0 means os.Getppid().
	PPID int
	// Token is this registration's token; "" means $CROSSTALK_PI_TOKEN.
	Token string
	// SockDir holds the socket and status files; "" means DefaultSockDir().
	SockDir string
	// Host reports whether pid is a Pi host; nil means IsPiHost.
	Host func(pid int) bool
	// Directory supplies the descriptors Discover lists; nil means none.
	Directory interface {
		List() ([]registry.Descriptor, error)
	}
	// Alive reports whether pid runs; nil means signal 0 reaches it.
	Alive  func(pid int) bool
	Logger *slog.Logger
}

// Harness is Pi behind harness.Harness: the session is the one the parent Pi
// process proved over the shim's socket (ADR 0036).
type Harness struct {
	ppid    int
	token   string
	sockDir string
	host    func(int) bool
	dir     interface {
		List() ([]registry.Descriptor, error)
	}
	alive  func(int) bool
	logger *slog.Logger

	// acceptMu serializes hellos, so the different-session check and the
	// publication of proven are one step around register.
	acceptMu sync.Mutex

	mu      sync.Mutex
	proven  *hello
	claim   *hello
	nonce   string
	conn    net.Conn
	status  string
	waiters map[string]waiter
	// wsem makes write the one writer on the extension's connections.
	wsem chan struct{}
}

var (
	_ harness.Harness     = (*Harness)(nil)
	_ harness.ClaimSource = (*Harness)(nil)
	_ harness.Noticer     = (*Harness)(nil)
)

func NewHarness(opt Options) *Harness {
	h := &Harness{ppid: opt.PPID, token: opt.Token, sockDir: opt.SockDir, host: opt.Host, dir: opt.Directory, alive: opt.Alive, logger: opt.Logger, waiters: map[string]waiter{}, wsem: make(chan struct{}, 1)}
	if h.ppid == 0 {
		h.ppid = os.Getppid()
	}
	if h.token == "" {
		h.token = os.Getenv(TokenEnv)
	}
	if h.sockDir == "" {
		h.sockDir = DefaultSockDir()
	}
	if h.host == nil {
		h.host = IsPiHost
	}
	if h.alive == nil {
		h.alive = alive
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	return h
}

// DefaultSockDir is the directory the extension dials, fixed under the home
// directory because the extension computes the same path itself.
func DefaultSockDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "crosstalk", "sock", "pi")
}

// IsPiCommand reports that cmd, a process's whole command, is Pi's title.
func IsPiCommand(cmd string) bool { return cmd == "pi" }

// IsPiHost reports that this shim runs under Pi's environment and that pid's
// whole command is exactly "pi", the process.title Pi sets.
func IsPiHost(pid int) bool {
	if os.Getenv("PI_CODING_AGENT") != "true" || pid <= 1 {
		return false
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && IsPiCommand(strings.TrimSpace(string(out)))
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (h *Harness) sockPath() string {
	return filepath.Join(h.sockDir, strconv.Itoa(h.ppid)+".sock")
}

func (h *Harness) Name() adapter.Harness { return adapter.Pi }

func (h *Harness) Notice(c msg.NoticeClass, fromHarness string) string {
	return msg.PeerNotice(c, fromHarness)
}

func (h *Harness) NativeReplyAddress(harness.Self) string { return "" }

func (h *Harness) Register(context.Context, string) (adapter.Binding, error) {
	return adapter.Binding{}, errors.New("pi: a Pi session is reachable only through its own shim")
}

func (h *Harness) session() (hello, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.proven == nil {
		return hello{}, false
	}
	return *h.proven, true
}

// sessionFor is the session a hello is registering when req is that
// registration's own call (it carries the claim nonce), else the proven one.
func (h *Harness) sessionFor(req harness.RequestContext) (hello, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.claim != nil && req.Meta != nil && req.Meta[claimMeta] == h.nonce {
		return *h.claim, true
	}
	if h.proven != nil {
		return *h.proven, true
	}
	return hello{}, false
}

// Identify claims a call only from the proven parent once a hello is held.
func (h *Harness) Identify(_ context.Context, req harness.RequestContext) (harness.Session, bool, error) {
	p, ok := h.sessionFor(req)
	if !ok || req.ParentPID != h.ppid {
		return harness.Session{}, false, nil
	}
	return harness.Session{Harness: adapter.Pi, SessionRef: p.ref(), Name: p.Name, Cwd: p.Cwd, HarnessPID: h.ppid, Endpoint: h.sockPath()}, true, nil
}

func (h *Harness) Bind(_ context.Context, req harness.RequestContext) (adapter.Binding, bool, error) {
	p, ok := h.sessionFor(req)
	if !ok || req.ParentPID != h.ppid {
		return adapter.Binding{}, false, nil
	}
	return adapter.Binding{Harness: adapter.Pi, SessionRef: p.ref(), HarnessEndpoint: h.sockPath(), HarnessVersion: p.PiVersion, HarnessPID: h.ppid}, true, nil
}

func (h *Harness) Status(_ context.Context, sessionRef string) (adapter.SessionStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.proven == nil || h.proven.ref() != sessionRef {
		return adapter.SessionStatus{}, errors.New("pi: session not served by this shim")
	}
	return adapter.SessionStatus{Loaded: true, Busy: h.status == "busy", Reachable: h.conn != nil}, nil
}

// Discover lists every Pi descriptor whose shim and Pi process run, reachable
// while its owning shim's status file says the extension is connected.
func (h *Harness) Discover(context.Context) ([]harness.Session, error) {
	if h.dir == nil {
		return nil, nil
	}
	ds, err := h.dir.List()
	if err != nil {
		return nil, err
	}
	var out []harness.Session
	for _, d := range ds {
		if d.Harness != string(adapter.Pi) || d.HarnessPID == nil || !h.alive(d.PID) || !h.alive(*d.HarnessPID) {
			continue
		}
		s := harness.Session{Harness: adapter.Pi, SessionRef: d.SessionRef, Name: d.Name, Cwd: d.Cwd, HarnessPID: *d.HarnessPID}
		if d.HarnessEndpoint != nil {
			s.Endpoint = *d.HarnessEndpoint
		}
		if st, ok := readStatus(h.statusPath(*d.HarnessPID, d.PID)); ok {
			s.Reachable = true
			if st.Status == "busy" || st.Status == "idle" {
				s.Status = st.Status
			}
		}
		out = append(out, s)
	}
	return out, nil
}
