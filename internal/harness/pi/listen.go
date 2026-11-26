package pi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/peercred"
	"github.com/GoodOlClint/crosstalk/internal/quiesce"
)

// Proto is the line protocol version the extension and the shim speak.
const Proto = 1

// MaxLine bounds every extension line; a hello slower than helloTimeout is
// refused.
const (
	MaxLine = 64 << 10
)

var (
	writeTimeout = 5 * time.Second
	helloTimeout = 5 * time.Second
	listenRetry  = 200 * time.Millisecond
)

// Pi's own session id rule (assertValidSessionId in session-manager.js).
var sessionID = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

type hello struct {
	Proto       int    `json:"proto"`
	Token       string `json:"token"`
	ExtVersion  string `json:"ext_version"`
	PiVersion   string `json:"pi_version"`
	SessionID   string `json:"session_id"`
	SessionFile string `json:"session_file"`
	Cwd         string `json:"cwd"`
	Name        string `json:"name"`
}

// ref is the session ref: an explicit --session-id is scoped to a directory,
// so the id alone does not name one session (D3).
func (p hello) ref() string {
	if p.SessionFile == "" {
		return p.SessionID
	}
	sum := sha256.Sum256([]byte(p.SessionFile))
	return p.SessionID + "@" + hex.EncodeToString(sum[:])[:8]
}

// check validates everything but the session binding; the error text is the
// reason the extension shows.
func (h *Harness) check(p hello) error {
	if p.Proto != Proto {
		return fmt.Errorf("protocol %d not supported, this crosstalk speaks %d: update the extension with crosstalk install pi", p.Proto, Proto)
	}
	if h.token == "" || subtle.ConstantTimeCompare([]byte(p.Token), []byte(h.token)) != 1 {
		return errors.New("token mismatch: this shim was not started by this extension")
	}
	warn, err := CheckVersion(p.PiVersion)
	if err != nil {
		return err
	}
	if warn != "" {
		h.logger.Warn("pi: " + warn)
	}
	if !sessionID.MatchString(p.SessionID) {
		return errors.New("malformed session id")
	}
	return nil
}

// statusPath is per shim as well as per Pi process, so a shim never removes
// a successor's file.
func (h *Harness) statusPath(piPID, shimPID int) string {
	return filepath.Join(h.sockDir, strconv.Itoa(piPID)+"."+strconv.Itoa(shimPID)+".status")
}

type statusDoc struct {
	PID    int    `json:"pid"`
	Status string `json:"status"`
}

func readStatus(path string) (statusDoc, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return statusDoc{}, false
	}
	var st statusDoc
	return st, json.Unmarshal(b, &st) == nil
}

// writeStatus records the connection state for other shims' Discover; the
// caller holds h.mu.
func (h *Harness) writeStatus() {
	path := h.statusPath(h.ppid, os.Getpid())
	if h.conn == nil {
		os.Remove(path)
		return
	}
	st := h.status
	if st == "" {
		st = "connected"
	}
	b, _ := json.Marshal(statusDoc{PID: os.Getpid(), Status: st})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		h.logger.Warn("pi: writing status file", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		h.logger.Warn("pi: writing status file", "err", err)
	}
}

// ServeClaims listens for the parent Pi's extension and registers the session
// its hello proves. It returns at once unless the parent is a Pi host.
func (h *Harness) ServeClaims(ctx context.Context, r harness.SessionRegistrar) error {
	if !h.host(h.ppid) {
		return nil
	}
	if err := os.MkdirAll(h.sockDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(h.sockDir, 0o700); err != nil {
		return err
	}
	path := h.sockPath()
	ln, err := h.listen(ctx, path)
	if ln == nil {
		return err
	}
	own, _ := os.Stat(path)
	var wg sync.WaitGroup
	defer func() {
		// Unlinked while still listening: a successor waits for this listener
		// to stop answering before it replaces the path (listen).
		if fi, err := os.Stat(path); err == nil && own != nil && os.SameFile(fi, own) {
			os.Remove(path)
		}
		ln.Close()
		wg.Wait()
		h.mu.Lock()
		os.Remove(h.statusPath(h.ppid, os.Getpid()))
		h.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() { h.serve(ctx, c, r) })
	}
}

// listen takes the socket path once no listener answers on it. One that
// answers is a shim still running for this Pi process, as the old session's
// shim is for a moment after /new, so this one waits rather than displace it.
// A path that is not a socket is never removed. A nil listener with a nil
// error means ctx ended.
func (h *Harness) listen(ctx context.Context, path string) (*net.UnixListener, error) {
	for {
		busy := false
		fi, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, err
		case fi.Mode()&os.ModeSocket == 0:
			return nil, fmt.Errorf("pi: %s exists and is not a socket", path)
		default:
			if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
				c.Close()
				busy = true
			} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		if !busy {
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err == nil {
				ln.SetUnlinkOnClose(false)
				return ln, nil
			}
			if !errors.Is(err, syscall.EADDRINUSE) {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(listenRetry):
		}
	}
}

var errUnterminated = errors.New("line not terminated")

// lines splits on '\n' only: an unterminated tail at EOF or at the read
// deadline is an error, never a line.
func lines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errUnterminated
	}
	return 0, nil, nil
}

var errWriteBusy = errors.New("another write to the extension did not finish")

// write is the only writer on an extension connection: one line waits at
// most writeTimeout to start and writeTimeout to finish. A failed write
// closes c, because a partial line would join the next one.
func (h *Harness) write(c net.Conn, line []byte) error {
	t := time.NewTimer(writeTimeout)
	defer t.Stop()
	select {
	case h.wsem <- struct{}{}:
	case <-t.C:
		return errWriteBusy
	}
	defer func() { <-h.wsem }()
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := c.Write(line); err != nil {
		c.Close()
		return err
	}
	return nil
}

func (h *Harness) reply(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	h.write(c, append(b, '\n'))
}

func (h *Harness) refuse(c net.Conn, reason string) {
	h.reply(c, map[string]string{"error": reason})
}

// serve runs one connection: the peer check before any read, the hello, then
// status and name lines until it drops.
func (h *Harness) serve(ctx context.Context, c *net.UnixConn, r harness.SessionRegistrar) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if pid, err := peercred.PeerPID(c); err != nil || pid != h.ppid {
		h.logger.Warn("pi: closed a connection from a process other than the parent", "peer_pid", pid, "ppid", h.ppid, "err", err)
		return
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), MaxLine)
	sc.Split(lines)
	c.SetReadDeadline(time.Now().Add(helloTimeout))
	if !sc.Scan() {
		h.logger.Warn("pi: no hello", "err", sc.Err())
		h.refuse(c, "no complete hello line within the limits")
		return
	}
	var p hello
	if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
		h.refuse(c, "malformed hello")
		return
	}
	if err := h.check(p); err != nil {
		h.logger.Warn("pi: hello refused", "err", err)
		h.refuse(c, err.Error())
		return
	}
	if err := h.accept(ctx, c, p, r); err != nil {
		h.logger.Warn("pi: hello refused", "session", p.SessionID, "err", err)
		h.refuse(c, err.Error())
		return
	}
	c.SetReadDeadline(time.Time{})
	defer h.drop(c)
	for sc.Scan() && h.line(ctx, c, sc.Bytes(), r) {
	}
}

// accept binds the hello's session to this shim and registers it. A shim
// serves one session for its life (ADR 0036 point 5): a hello for another
// session is refused and the bound one stays. Until register succeeds only
// the registration's own calls see the session (sessionFor).
func (h *Harness) accept(ctx context.Context, c net.Conn, p hello, r harness.SessionRegistrar) error {
	h.acceptMu.Lock()
	defer h.acceptMu.Unlock()
	nonce := rand.Text()
	h.mu.Lock()
	if h.proven != nil && h.proven.ref() != p.ref() {
		h.mu.Unlock()
		return fmt.Errorf("session %s is not the session this shim serves", p.SessionID)
	}
	h.claim, h.nonce = &p, nonce
	h.mu.Unlock()
	err := register(ctx, r, harness.RequestContext{ParentPID: h.ppid, Meta: map[string]any{claimMeta: nonce}}, p.Cwd)
	h.mu.Lock()
	h.claim, h.nonce = nil, ""
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("session not registered: %v", err)
	}
	h.proven = &p
	if h.conn != nil && h.conn != c {
		h.conn.Close()
	}
	h.conn = c
	h.writeStatus()
	h.mu.Unlock()
	h.reply(c, map[string]string{"hello": "ok"})
	return nil
}

// claimMeta is the RequestContext.Meta key that marks a hello's own
// registration calls.
const claimMeta = "crosstalk.pi/claim"

// register runs inside the refresh gate, so no descriptor changes while a
// refreshing shim takes its handoff.
func register(ctx context.Context, r harness.SessionRegistrar, req harness.RequestContext, cwd string) error {
	done, ok := quiesce.Hold(ctx)
	if !ok {
		return ctx.Err()
	}
	defer done()
	return r.RegisterSession(ctx, req, cwd)
}

func (h *Harness) drop(c net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == c {
		h.conn = nil
		h.writeStatus()
	}
	for id, w := range h.waiters {
		if w.conn == c {
			delete(h.waiters, id)
			close(w.ch)
		}
	}
}

// line handles one extension line after the hello: a delivery answer, a
// status, a session name, or a repeated hello. Lines from a connection the session no longer uses are
// dropped. It returns false when the connection must close: a hello naming
// another session.
func (h *Harness) line(ctx context.Context, c net.Conn, b []byte, r harness.SessionRegistrar) bool {
	var f map[string]json.RawMessage
	if json.Unmarshal(b, &f) != nil {
		h.logger.Warn("pi: malformed line from the extension")
		return true
	}
	if _, ok := f["session_id"]; ok {
		var p hello
		cur, _ := h.session()
		if json.Unmarshal(b, &p) != nil || p.ref() != cur.ref() {
			h.logger.Warn("pi: hello for another session refused", "session", p.SessionID, "bound", cur.SessionID)
			h.refuse(c, fmt.Sprintf("session %s is not the session this shim serves", p.SessionID))
			return false
		}
		return true
	}
	if _, ok := f["id"]; ok {
		h.answer(c, f)
		return true
	}
	if raw, ok := f["status"]; ok {
		var st string
		if json.Unmarshal(raw, &st) == nil && (st == "busy" || st == "idle") {
			h.mu.Lock()
			if h.conn == c {
				h.status = st
				h.writeStatus()
			}
			h.mu.Unlock()
		}
		return true
	}
	if raw, ok := f["name"]; ok {
		var name string
		if json.Unmarshal(raw, &name) != nil {
			return true
		}
		h.mu.Lock()
		if h.proven == nil || h.conn != c {
			h.mu.Unlock()
			return true
		}
		p := *h.proven
		p.Name = name
		h.proven = &p
		h.mu.Unlock()
		if err := register(ctx, r, harness.RequestContext{ParentPID: h.ppid}, p.Cwd); err != nil {
			h.logger.Warn("pi: applying session name", "err", err)
		}
	}
	return true
}
