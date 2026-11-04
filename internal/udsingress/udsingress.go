package udsingress

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/peercred"
	"github.com/GoodOlClint/crosstalk/internal/procstart"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const (
	DefaultMaxFrame    = 1 << 20
	DefaultReadTimeout = 10 * time.Second
)

var (
	ErrKeyMode   = errors.New("udsingress: key file is not mode 0600")
	ErrKeyEmpty  = errors.New("udsingress: key file is empty")
	ErrInUse     = errors.New("udsingress: socket has a live listener")
	errTooLarge  = errors.New("udsingress: frame exceeds limit")
	errBadFrame  = errors.New("udsingress: malformed frame")
	errBadAuth   = errors.New("udsingress: auth failed")
	errNoContent = errors.New("udsingress: user frame has no text content")
)

type Directory interface {
	List() ([]registry.Descriptor, error)
}

type Receiver interface {
	Receive(ctx context.Context, in receiver.Inbound) receiver.Result
}

type Server struct {
	Path        string
	KeyPath     string
	Directory   Directory
	Receiver    Receiver
	StartTime   func(pid int) (time.Time, error)
	PeerPID     func(*net.UnixConn) (int, error)
	MaxFrame    int
	ReadTimeout time.Duration

	mu    sync.Mutex
	ln    *net.UnixListener
	token string
	wg    sync.WaitGroup
}

// KeyPath is the CrossTalk-issued key next to an agent socket: <uds_path
// without .sock>.key.
func KeyPath(udsPath string) string {
	return strings.TrimSuffix(udsPath, ".sock") + ".key"
}

// EnsureKey returns the token in path, creating a fresh 0600 key if the file
// does not exist. A key readable by anyone but the owner is refused.
func EnsureKey(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, werr := f.WriteString(hex.EncodeToString(raw[:]) + "\n")
		if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
			os.Remove(path)
			return "", err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("%w: %s %v", ErrKeyMode, path, fi.Mode())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", ErrKeyEmpty
	}
	return t, nil
}

// Listen binds Path. A leftover socket with no listener is replaced; the
// caller holds the agent record lock, so no other registration owns it.
func (s *Server) Listen() error {
	if s.KeyPath == "" {
		s.KeyPath = KeyPath(s.Path)
	}
	tok, err := EnsureKey(s.KeyPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(s.Path); err == nil {
		if c, err := net.DialTimeout("unix", s.Path, time.Second); err == nil {
			c.Close()
			return fmt.Errorf("%w: %s", ErrInUse, s.Path)
		}
		if err := os.Remove(s.Path); err != nil {
			return err
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.Path, Net: "unix"})
	if err != nil {
		return err
	}
	if err := os.Chmod(s.Path, 0o600); err != nil {
		ln.Close()
		return err
	}
	s.mu.Lock()
	s.ln, s.token = ln, tok
	s.mu.Unlock()
	return nil
}

// Serve accepts connections until ctx ends or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("udsingress: Serve before Listen")
	}
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			s.wg.Wait()
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.wg.Go(func() { s.handle(ctx, c) })
	}
}

func (s *Server) Close() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return nil
	}
	err := ln.Close()
	s.wg.Wait()
	return err
}

func (s *Server) peerPID(c *net.UnixConn) (int, error) {
	if s.PeerPID != nil {
		return s.PeerPID(c)
	}
	return peercred.PeerPID(c)
}

func (s *Server) startTime(pid int) (time.Time, error) {
	if s.StartTime != nil {
		return s.StartTime(pid)
	}
	return procstart.StartTime(pid)
}

type ack struct {
	Type      string `json:"type"`
	MessageID string `json:"message_id,omitempty"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

func writeAck(c net.Conn, a ack) {
	a.Type = "ack"
	b, _ := json.Marshal(a)
	c.Write(append(b, '\n'))
}

func (s *Server) handle(ctx context.Context, c *net.UnixConn) {
	defer c.Close()
	pid, perr := s.peerPID(c)
	var started time.Time
	var serr error
	if perr == nil {
		started, serr = s.startTime(pid)
	}

	rt := s.ReadTimeout
	if rt == 0 {
		rt = DefaultReadTimeout
	}
	c.SetDeadline(time.Now().Add(rt))
	max := s.MaxFrame
	if max == 0 {
		max = DefaultMaxFrame
	}
	r := bufio.NewReader(c)

	line, err := readLine(r, max)
	if err != nil {
		return
	}
	var sender receiver.Sender
	if lineType(line) == "user" {
		if perr != nil || serr != nil {
			return
		}
		sender = s.identify(pid, started)
		if !sender.Verified {
			return
		}
	} else {
		if !s.authOK(line) {
			return
		}
		line, err = readLine(r, max)
		if errors.Is(err, errTooLarge) {
			writeAck(c, ack{State: string(adapter.Rejected), Reason: receiver.ReasonTooLarge})
			return
		}
		if err != nil {
			return
		}
		sender = receiver.Sender{PID: pid}
		if perr == nil && serr == nil {
			sender = s.identify(pid, started)
		}
	}
	f, err := ParseUserFrame(line)
	if err != nil {
		writeAck(c, ack{State: string(adapter.Rejected), Reason: receiver.ReasonInvalid})
		return
	}

	kind := msg.Inform
	if f.Kind != "" {
		kind = msg.Kind(f.Kind)
	}
	in := receiver.Inbound{
		Ingress: receiver.UDS,
		Sender:  sender,
		Native:  true,
		Message: msg.Message{Kind: kind, Delivery: msg.Immediate, HopChain: hopChain(f), Parts: []msg.Part{{Type: "text", Text: f.Body}}},
	}
	c.SetDeadline(time.Time{})
	res := s.Receiver.Receive(ctx, in)
	c.SetWriteDeadline(time.Now().Add(rt))
	writeAck(c, ack{MessageID: res.MessageID, State: string(res.State), Reason: res.Reason})
}

func lineType(line []byte) string {
	var t struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &t) != nil {
		return ""
	}
	return t.Type
}

func hopChain(f Frame) []string {
	h := f.Attrs["hop-chain"]
	if h == "" {
		return nil
	}
	chain := strings.Split(h, ",")
	if msg.CheckHopChain(chain) != nil {
		return nil
	}
	return chain
}

func (s *Server) authOK(line []byte) bool {
	var a struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if json.Unmarshal(line, &a) != nil || a.Type != "auth" {
		return false
	}
	s.mu.Lock()
	tok := s.token
	s.mu.Unlock()
	return tok != "" && subtle.ConstantTimeCompare([]byte(a.Token), []byte(tok)) == 1
}

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > max {
			return nil, errTooLarge
		}
		if err == nil {
			return buf[:len(buf)-1], nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
}

// identify maps a peer pid to exactly one live descriptor whose pid or
// harness_pid it is, with a matching process start time (§5 V18). No match,
// a start-time mismatch, or more than one candidate (a pooled shim) is
// unverified; from-name is never consulted.
func (s *Server) identify(pid int, started time.Time) receiver.Sender {
	unverified := receiver.Sender{PID: pid}
	ds, err := s.Directory.List()
	if err != nil {
		return unverified
	}
	var cand []registry.Descriptor
	for _, d := range ds {
		if d.PID == pid || (d.HarnessPID != nil && *d.HarnessPID == pid) {
			cand = append(cand, d)
		}
	}
	if len(cand) != 1 {
		return unverified
	}
	d := cand[0]
	ok := (d.PID == pid && d.StartedAt.Equal(started)) ||
		(d.HarnessPID != nil && *d.HarnessPID == pid && d.HarnessStartedAt != nil && d.HarnessStartedAt.Equal(started))
	if !ok {
		return unverified
	}
	role := d.Role
	if role == "" {
		role = registry.RolePrimary
	}
	return receiver.Sender{Verified: true, AgentID: d.AgentID, Name: d.Name, Role: role, PID: pid}
}
