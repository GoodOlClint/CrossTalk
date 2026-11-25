package pi

import (
	"bufio"
	"context"
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
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/peercred"
)

// Proto is the line protocol version the extension and the shim speak.
const Proto = 1

// MaxLine bounds every extension line; a hello slower than helloTimeout is
// refused.
const (
	MaxLine      = 64 << 10
	writeTimeout = 5 * time.Second
)

var helloTimeout = 5 * time.Second

// Pi's own session id rule (session-manager.js): alphanumerics, '.', '_', '-'.
var sessionID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)

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

func (h *Harness) statusPath(pid int) string {
	return filepath.Join(h.sockDir, strconv.Itoa(pid)+".status")
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
	path := h.statusPath(h.ppid)
	if h.conn == nil {
		removeOwnStatus(path)
		return
	}
	st := h.status
	if st == "" {
		st = "connected"
	}
	b, _ := json.Marshal(statusDoc{PID: os.Getpid(), Status: st})
	tmp := path + ".tmp" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		h.logger.Warn("pi: writing status file", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		h.logger.Warn("pi: writing status file", "err", err)
	}
}

// removeOwnStatus removes the status file only while it names this shim: a
// newer shim for the same Pi process may have written it.
func removeOwnStatus(path string) {
	if st, ok := readStatus(path); ok && st.PID == os.Getpid() {
		os.Remove(path)
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
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	ln.SetUnlinkOnClose(false)
	own, _ := os.Stat(path)
	var wg sync.WaitGroup
	defer func() {
		ln.Close()
		h.mu.Lock()
		if h.conn != nil {
			h.conn.Close()
		}
		h.mu.Unlock()
		wg.Wait()
		if fi, err := os.Stat(path); err == nil && own != nil && os.SameFile(fi, own) {
			os.Remove(path)
		}
		removeOwnStatus(h.statusPath(h.ppid))
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

func reply(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	c.Write(append(b, '\n'))
}

func refuse(c net.Conn, reason string) {
	reply(c, map[string]string{"error": reason})
}

// serve runs one connection: the peer check before any read, the hello, then
// status and name lines until it drops.
func (h *Harness) serve(ctx context.Context, c *net.UnixConn, r harness.SessionRegistrar) {
	defer c.Close()
	if pid, err := peercred.PeerPID(c); err != nil || pid != h.ppid {
		h.logger.Warn("pi: closed a connection from a process other than the parent", "peer_pid", pid, "ppid", h.ppid, "err", err)
		return
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), MaxLine)
	c.SetReadDeadline(time.Now().Add(helloTimeout))
	if !sc.Scan() {
		h.logger.Warn("pi: no hello", "err", sc.Err())
		refuse(c, "no complete hello line within the limits")
		return
	}
	var p hello
	if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
		refuse(c, "malformed hello")
		return
	}
	if err := h.check(p); err != nil {
		h.logger.Warn("pi: hello refused", "err", err)
		refuse(c, err.Error())
		return
	}
	if err := h.accept(ctx, c, p, r); err != nil {
		h.logger.Warn("pi: hello refused", "session", p.SessionID, "err", err)
		refuse(c, err.Error())
		return
	}
	c.SetReadDeadline(time.Time{})
	defer h.drop(c)
	for sc.Scan() {
		h.line(ctx, c, sc.Bytes(), r)
	}
}

// accept binds the hello's session to this shim and registers it. A shim
// serves one session for its life (ADR 0036 point 5): a hello for another
// session is refused and the bound one stays.
func (h *Harness) accept(ctx context.Context, c net.Conn, p hello, r harness.SessionRegistrar) error {
	h.mu.Lock()
	prev := h.proven
	if prev != nil && prev.ref() != p.ref() {
		h.mu.Unlock()
		return fmt.Errorf("session %s is not the session this shim serves", p.SessionID)
	}
	h.proven = &p
	h.mu.Unlock()
	if err := r.RegisterSession(ctx, harness.RequestContext{ParentPID: h.ppid}, p.Cwd); err != nil {
		h.mu.Lock()
		if h.proven == &p {
			h.proven = prev
		}
		h.mu.Unlock()
		return fmt.Errorf("session not registered: %v", err)
	}
	h.mu.Lock()
	if h.conn != nil && h.conn != c {
		h.conn.Close()
	}
	h.conn = c
	h.writeStatus()
	h.mu.Unlock()
	reply(c, map[string]string{"hello": "ok"})
	return nil
}

func (h *Harness) drop(c net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == c {
		h.conn = nil
		h.writeStatus()
	}
}

// line handles one extension line after the hello: a status, a session name,
// or a repeated hello. Anything else is ignored here.
func (h *Harness) line(ctx context.Context, c net.Conn, b []byte, r harness.SessionRegistrar) {
	var f map[string]json.RawMessage
	if json.Unmarshal(b, &f) != nil {
		h.logger.Warn("pi: malformed line from the extension")
		return
	}
	if _, ok := f["session_id"]; ok {
		var p hello
		if json.Unmarshal(b, &p) != nil {
			refuse(c, "malformed hello")
			return
		}
		cur, _ := h.session()
		if p.ref() != cur.ref() {
			h.logger.Warn("pi: hello for another session refused", "session", p.SessionID, "bound", cur.SessionID)
			refuse(c, fmt.Sprintf("session %s is not the session this shim serves", p.SessionID))
		}
		return
	}
	if raw, ok := f["status"]; ok {
		var st string
		if json.Unmarshal(raw, &st) == nil && (st == "busy" || st == "idle") {
			h.mu.Lock()
			h.status = st
			if h.conn == c {
				h.writeStatus()
			}
			h.mu.Unlock()
		}
		return
	}
	if raw, ok := f["name"]; ok {
		var name string
		if json.Unmarshal(raw, &name) != nil {
			return
		}
		h.mu.Lock()
		if h.proven == nil {
			h.mu.Unlock()
			return
		}
		p := *h.proven
		p.Name = name
		h.proven = &p
		h.mu.Unlock()
		if err := r.RegisterSession(ctx, harness.RequestContext{ParentPID: h.ppid}, p.Cwd); err != nil {
			h.logger.Warn("pi: applying session name", "err", err)
		}
	}
}
