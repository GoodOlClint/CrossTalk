package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
	"github.com/GoodOlClint/crosstalk/internal/peercred"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// SettingFile, present in the state directory, turns on the experimental
// --no-daemon claim handoff for every Codex session of this user on this
// host. Absent, the hook and the shim do nothing.
const SettingFile = "codex-no-daemon.on"

const (
	DefaultRecheckInterval = 10 * time.Second
	claimReadLimit         = 64 << 10
	connBound              = 2 * time.Second
)

func ClaimsEnabled(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, SettingFile))
	return err == nil
}

func ClaimDir(stateDir string) string { return filepath.Join(stateDir, "codex-claims") }

// DefaultSockDir is short so that every claim socket fits sun_path (104
// bytes on macOS) whatever the home directory's length.
func DefaultSockDir() string { return "/tmp/crosstalk-" + strconv.Itoa(os.Geteuid()) }

func ownerKey(p ownerproof.Proc) string {
	return strconv.Itoa(p.PID) + "-" + strconv.FormatInt(p.Start.UnixMicro(), 10)
}

// ClaimSocketPath is where the shim under owner listens for its hook's
// claims; both sides derive it from the owner's pid and start time.
func ClaimSocketPath(sockDir string, owner ownerproof.Proc) string {
	return filepath.Join(sockDir, "cx-"+ownerKey(owner)+".sock")
}

func claimFile(stateDir string, owner ownerproof.Proc, sessionID string) string {
	return filepath.Join(ClaimDir(stateDir), ownerKey(owner)+"-"+sessionID+".json")
}

func isCodex(exe string, argv []string) bool {
	return filepath.Base(exe) == "codex" || (len(argv) > 0 && filepath.Base(argv[0]) == "codex")
}

// ensurePrivateDir creates dir 0700 and refuses one that is a symlink, not
// a directory, not owned by this user, or open to others.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 || !ownedBySelf(fi) {
		return fmt.Errorf("codex claim: %s is not a private directory of this user", dir)
	}
	return nil
}

// ClaimServer is the shim side of the handoff: the live socket, the scan of
// durable claims for this shim's parent, and the periodic recheck.
type ClaimServer struct {
	Claims   *Claims
	Owner    ownerproof.Proc
	StateDir string
	SockDir  string
	PeerPID  func(*net.UnixConn) (int, error)
	Interval time.Duration
	Logger   *slog.Logger

	// failed holds, per session, the At of a durable claim that failed, so
	// each written claim is tried once per shim; a refire writes a new At.
	failed map[string]time.Time
}

func (s *ClaimServer) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Serve runs until ctx ends. A socket it cannot take (another live shim
// under the same parent holds it) leaves the durable path working.
func (s *ClaimServer) Serve(ctx context.Context) error {
	if s.PeerPID == nil {
		s.PeerPID = peercred.PeerPID
	}
	if s.Interval <= 0 {
		s.Interval = DefaultRecheckInterval
	}
	ln, err := s.listen()
	if err != nil {
		s.log().Warn("codex claim: no live socket; durable claims only", "err", err)
	} else {
		path := ln.Addr().String()
		ln.SetUnlinkOnClose(false)
		defer os.Remove(path)
		defer ln.Close()
		go s.accept(ctx, ln)
	}
	s.Scan(ctx)
	tick := time.NewTicker(s.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			s.Claims.Recheck()
			s.Scan(ctx)
		}
	}
}

func (s *ClaimServer) listen() (*net.UnixListener, error) {
	if err := ensurePrivateDir(s.SockDir); err != nil {
		return nil, err
	}
	path := ClaimSocketPath(s.SockDir, s.Owner)
	if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("codex claim: %s is served by another process", path)
	}
	os.Remove(path)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (s *ClaimServer) accept(ctx context.Context, ln *net.UnixListener) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		go s.handle(ctx, conn)
	}
}

func (s *ClaimServer) handle(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(connBound))
	pid, err := s.PeerPID(conn)
	if err != nil {
		s.log().Info("codex claim: no peer pid", "err", err)
		return
	}
	line, err := bufio.NewReader(io.LimitReader(conn, claimReadLimit)).ReadBytes('\n')
	if err != nil {
		return
	}
	var cl Claim
	if json.Unmarshal(line, &cl) != nil {
		return
	}
	reply := "ok\n"
	if err := s.Claims.AcceptLive(ctx, pid, cl); err != nil {
		s.log().Info("codex claim ignored", "thread", cl.SessionID, "sender", pid, "err", err)
		reply = "ignored\n"
	}
	conn.Write([]byte(reply))
}

// Scan accepts each durable claim file written for this shim's parent that
// is not already bound as written.
func (s *ClaimServer) Scan(ctx context.Context) {
	dir := ClaimDir(s.StateDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := ownerKey(s.Owner) + "-"
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := readLimited(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var cl Claim
		if json.Unmarshal(b, &cl) != nil || name != prefix+cl.SessionID+".json" {
			continue
		}
		if cur, ok := s.Claims.Binding(cl.SessionID); ok && cur.Claim.At.Equal(cl.At) {
			continue
		}
		if at, ok := s.failed[cl.SessionID]; ok && at.Equal(cl.At) {
			continue
		}
		if err := s.Claims.AcceptDurable(ctx, cl); err != nil {
			if s.failed == nil {
				s.failed = map[string]time.Time{}
			}
			s.failed[cl.SessionID] = cl.At
			s.log().Info("codex durable claim ignored", "thread", cl.SessionID, "err", err)
		}
	}
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, claimReadLimit))
}

// ClaimsConfig is what Harness.ServeClaims reads; zero fields mean the
// production values.
type ClaimsConfig struct {
	StateDir string
	SockDir  string
	// CodexHome holds thread-writer-locks; "" means $CODEX_HOME or ~/.codex.
	CodexHome string
	Sys       System
	// ParentPID is the shim's parent; 0 means os.Getppid().
	ParentPID int
	Interval  time.Duration
}

var _ harness.ClaimSource = (*Harness)(nil)

// ServeClaims serves the SessionStart claim handoff while the setting is on
// and this shim's parent is a codex process; otherwise it returns at once.
func (h *Harness) ServeClaims(ctx context.Context, r harness.SessionRegistrar) error {
	cfg := h.claims
	if cfg.StateDir == "" {
		d, err := registry.DefaultDir()
		if err != nil {
			return err
		}
		cfg.StateDir = d
	}
	if !ClaimsEnabled(cfg.StateDir) {
		return nil
	}
	if cfg.Sys == nil {
		cfg.Sys = Kernel{}
	}
	if cfg.SockDir == "" {
		cfg.SockDir = DefaultSockDir()
	}
	if cfg.ParentPID == 0 {
		cfg.ParentPID = os.Getppid()
	}
	if cfg.CodexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		cfg.CodexHome = codexHome(home)
	}
	parent, err := cfg.Sys.Info(cfg.ParentPID)
	if err != nil {
		return err
	}
	exe, argv, err := cfg.Sys.Args(parent.PID)
	if err != nil || !isCodex(exe, argv) {
		return err
	}
	c := NewClaims(ClaimOptions{Sys: cfg.Sys, Parent: parent, LockDir: filepath.Join(cfg.CodexHome, "thread-writer-locks"), Registrar: r, Logger: h.logger})
	srv := &ClaimServer{Claims: c, Owner: parent, StateDir: cfg.StateDir, SockDir: cfg.SockDir, Interval: cfg.Interval, Logger: h.logger}
	return srv.Serve(ctx)
}
