// Package harness is the contract every agent harness implements (ADR 0008).
// internal/shim, internal/mcpserver, internal/a2a and internal/wire use only
// this package; cmd/crosstalk is the one importer of the concrete harness
// packages and lists them in trust order.
package harness

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// Backend is BackendReachable, BackendUnreachable or BackendUnknown for
	// a session whose harness can tell; "" otherwise.
	Backend string
}

const (
	BackendReachable   = "reachable"
	BackendUnreachable = "unreachable"
	BackendUnknown     = "unknown"
)

// BackendChecker is implemented by a harness that can say whether the
// service hosting session sessionRef at endpoint reaches its model backend.
type BackendChecker interface {
	Backend(ctx context.Context, endpoint, sessionRef string) string
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

// Installer is implemented by a harness that `crosstalk install` can set up
// (ADR 0016). InstallSteps and UninstallSteps only read: every change happens
// in a Step's Apply, after the user saw its Change.
type Installer interface {
	// Found reports that the harness CLI is on the PATH.
	Found() bool
	InstallSteps(env InstallEnv) []Step
	UninstallSteps(env InstallEnv) []Step
	Checks(ctx context.Context, env InstallEnv) []CheckResult
	// Registered reports, from the harness's config files alone, whether its
	// CrossTalk server runs env.Bin; it works without the harness CLI. An
	// error means it cannot tell, and uninstall then keeps the binary.
	Registered(env InstallEnv) (bool, error)
	// SetUp reports whether the harness has any CrossTalk setup: a crosstalk
	// server entry naming any command, or the CrossTalk plugin installed.
	// An error means it cannot tell, and callers treat that as set up.
	SetUp(env InstallEnv) (bool, error)
}

// Leftover is implemented by a harness whose uninstall leaves CrossTalk
// config in place (Claude's local- and project-scope entries). Uninstall keeps
// the binary and state while it reports a description or an error. With anyBin
// false only config that runs env.Bin counts (the binary step); with it true
// any CrossTalk config does (the state step).
type Leftover interface {
	Leftover(env InstallEnv, anyBin bool) (string, error)
}

type InstallEnv struct {
	// Bin is the absolute path of the installed crosstalk binary.
	Bin, Version, Home string
	Run                func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Handshake starts command with args and env exactly as given and checks
	// that it serves the CrossTalk tools at Version.
	Handshake func(ctx context.Context, command string, args, env []string) error
	// SkipRulePort lets install continue past old-prefix rules it cannot
	// edit; the checks then warn about each.
	SkipRulePort bool
	// Marketplace is what each harness's `plugin marketplace add` is given:
	// owner/repo or a local directory holding a marketplace named crosstalk.
	Marketplace string
}

type Step struct {
	Title, Change string
	Apply         func(ctx context.Context) error
}

type CheckResult struct {
	ID, Detail   string
	OK, WarnOnly bool
}

var backedUp sync.Map

// ReplaceFile writes data to path through a temp file renamed over it. The
// first call for a path in this process copies the current contents, if
// any, to path + ".crosstalk-backup", so the backup is the file as it was
// before this run. The file keeps its mode; a new file gets 0600.
func ReplaceFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if old, err := os.ReadFile(path); err == nil {
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
		if _, done := backedUp.LoadOrStore(path, true); !done {
			if err := os.WriteFile(path+".crosstalk-backup", old, mode); err != nil {
				backedUp.Delete(path)
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// PluginAcceptable is the K1 comparison. A final binary needs the plugin at
// its own version. A prerelease binary ("0.1.4-rc.1") also accepts the
// plugin at its base release or any older one, because the marketplace ref
// stays on the last final tag until the final tag moves it (ADR 0019).
func PluginAcceptable(binary, plugin string) bool {
	if binary == plugin {
		return true
	}
	i := strings.Index(binary, "-")
	if i < 0 || !releaseVersion(plugin) || !releaseVersion(binary[:i]) {
		return false
	}
	return plugin == binary[:i] || VersionLess(plugin, binary[:i])
}

// releaseVersion reports whether v is exactly X.Y.Z with decimal parts.
func releaseVersion(v string) bool {
	ps := strings.Split(v, ".")
	if len(ps) != 3 {
		return false
	}
	for _, p := range ps {
		if _, err := strconv.ParseUint(p, 10, 32); err != nil {
			return false
		}
	}
	return true
}

func VersionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x < y
		}
	}
	return len(as) < len(bs)
}

const RCNote = " (a prerelease binary installs the last release's plugin; to test the rc plugin, point CROSSTALK_MARKETPLACE at a scratch marketplace whose ref is the rc tag, see docs/setup.md)"
