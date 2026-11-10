// Package prebind keeps the informs a sender's shim injected into a session
// that had no CrossTalk binding, so the session's shim can replay them into
// its journal when it binds (ADR 0026, ADR 0023). Each entry is signed with
// the sender's own agent token.
package prebind

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

const (
	Version        = 1
	MaxEntries     = 64
	MaxTargetBytes = 4 << 20
	MaxFiles       = 256
	MaxDirBytes    = 16 << 20

	dataSuffix    = ".jsonl"
	claimedSuffix = ".claimed"
	lockName      = ".lock"
)

var ErrCap = errors.New("prebind: store cap reached")

// Entry is one stored delivery. Message is the JSON of the msg.Message the
// sender journaled; it is a string so the signed bytes survive a round trip.
type Entry struct {
	V           int       `json:"v"`
	AgentID     string    `json:"agent_id"`
	Harness     string    `json:"harness"`
	SessionRef  string    `json:"session_ref"`
	Sender      string    `json:"sender"`
	Message     string    `json:"message"`
	DeliveredAt time.Time `json:"delivered_at"`
	State       string    `json:"state"`
	MAC         string    `json:"mac"`
}

// Target is the binding a draining shim holds; an entry must name it exactly.
type Target struct {
	AgentID    string
	Harness    string
	SessionRef string
}

type Options struct {
	Now    func() time.Time
	TTL    time.Duration
	Logger *slog.Logger
}

type Store struct {
	dir    string
	tokens string
	now    func() time.Time
	ttl    time.Duration
	log    *slog.Logger
}

// New returns the store under stateDir/prebind; sender tokens are read from
// stateDir/tokens/<agent_id>.token.
func New(stateDir string, opt Options) *Store {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.TTL == 0 {
		opt.TTL = journal.InformRouteTTL
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	return &Store{dir: filepath.Join(stateDir, "prebind"), tokens: filepath.Join(stateDir, "tokens"), now: opt.Now, ttl: opt.TTL, log: opt.Logger}
}

// MessageID returns the id of the message an entry carries.
func (e Entry) MessageID() string {
	var m struct {
		MessageID string `json:"message_id"`
	}
	json.Unmarshal([]byte(e.Message), &m)
	return m.MessageID
}

func isAgentID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Store) dataPath(id string) string    { return filepath.Join(s.dir, id+dataSuffix) }
func (s *Store) claimedPath(id string) string { return filepath.Join(s.dir, id+claimedSuffix) }

func (s *Store) readToken(agentID string) (string, error) {
	f, err := os.OpenFile(filepath.Join(s.tokens, agentID+".token"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return "", errors.New("token is not a 0600 regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("token is empty")
	}
	return t, nil
}

func sign(token string, e Entry) string {
	h := hmac.New(sha256.New, []byte(token))
	field := func(v string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write([]byte(v))
	}
	field("crosstalk-prebind")
	field(strconv.Itoa(e.V))
	field(e.AgentID)
	field(e.Harness)
	field(e.SessionRef)
	field(e.Sender)
	field(e.Message)
	field(strconv.FormatInt(e.DeliveredAt.UnixNano(), 10))
	field(e.State)
	return hex.EncodeToString(h.Sum(nil))
}

func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Getuid())
}

func deliveredState(state string) bool { return state == "accepted" || state == "queued" }

// lock takes the store's one advisory lock. With create, it makes the
// directory; without it, a missing directory is os.ErrNotExist.
func (s *Store) lock(create bool) (func(), error) {
	if create {
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return nil, err
		}
	}
	fi, err := os.Lstat(s.dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 || !ownedByUs(fi) {
		return nil, fmt.Errorf("prebind: %s is not a 0700 directory owned by this user", s.dir)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, lockName), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

type usage struct {
	files int
	bytes int64
}

// sweep deletes data and claimed files whose last write is older than the
// TTL and returns what the rest use. The caller holds the lock.
func (s *Store) sweep() (usage, error) {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return usage{}, err
	}
	var u usage
	now := s.now()
	for _, de := range des {
		n := de.Name()
		if !strings.HasSuffix(n, dataSuffix) && !strings.HasSuffix(n, claimedSuffix) {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) > s.ttl {
			os.Remove(filepath.Join(s.dir, n))
			continue
		}
		u.files++
		u.bytes += fi.Size()
	}
	return u, nil
}

// Append signs e with the sender's own token and appends it to the target's
// file. ErrCap means nothing was written.
func (s *Store) Append(e Entry) error {
	if !isAgentID(e.AgentID) || !isAgentID(e.Sender) || e.Message == "" || e.DeliveredAt.IsZero() || !deliveredState(e.State) {
		return errors.New("prebind: malformed entry")
	}
	tok, err := s.readToken(e.Sender)
	if err != nil {
		return fmt.Errorf("prebind: sender token: %w", err)
	}
	e.V = Version
	e.DeliveredAt = e.DeliveredAt.UTC()
	e.MAC = sign(tok, e)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	unlock, err := s.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	u, err := s.sweep()
	if err != nil {
		return err
	}
	path := s.dataPath(e.AgentID)
	var have int64
	var entries int
	switch b, err := readRegular(path); {
	case err == nil:
		have, entries = int64(len(b)), bytes.Count(b, []byte{'\n'})
	case errors.Is(err, os.ErrNotExist):
		u.files++
	default:
		return err
	}
	if entries >= MaxEntries || have+int64(len(line)) > MaxTargetBytes || u.files > MaxFiles || u.bytes+int64(len(line)) > MaxDirBytes {
		return ErrCap
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// readRegular reads a 0600 regular file owned by this user, opened without
// following a symlink.
func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || !ownedByUs(fi) {
		return nil, fmt.Errorf("prebind: %s is not a 0600 regular file owned by this user", filepath.Base(path))
	}
	if fi.Size() > MaxDirBytes {
		return nil, fmt.Errorf("prebind: %s is over the size cap", filepath.Base(path))
	}
	return io.ReadAll(f)
}

// Drain hands imp every verified entry stored for t, oldest first. imp must
// be idempotent: a claimed file left by a failed or interrupted drain is read
// again in full by the next one. A failing imp stops the drain and keeps the
// claimed file. Dropped entries are logged and never returned.
func (s *Store) Drain(t Target, imp func(Entry) error) error {
	if !isAgentID(t.AgentID) {
		return errors.New("prebind: malformed target")
	}
	for pass := 0; pass < 2; pass++ {
		unlock, err := s.lock(false)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := s.sweep(); err != nil {
			unlock()
			return err
		}
		claimed := s.claimedPath(t.AgentID)
		_, cerr := os.Lstat(claimed)
		leftover := cerr == nil
		if !leftover {
			if err := os.Rename(s.dataPath(t.AgentID), claimed); err != nil {
				unlock()
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
		}
		unlock()
		if err := s.consume(t, claimed, imp); err != nil {
			return err
		}
		if !leftover {
			return nil
		}
	}
	return nil
}

func (s *Store) consume(t Target, claimed string, imp func(Entry) error) error {
	b, err := readRegular(claimed)
	if err != nil {
		s.log.Warn("prebind: dropping file", "file", filepath.Base(claimed), "err", err)
		return s.removeClaimed(claimed)
	}
	var kept []Entry
	senders := map[string]map[string]bool{}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			s.log.Warn("prebind: dropping entry", "reason", "malformed")
			continue
		}
		if reason := s.verify(t, e); reason != "" {
			s.log.Warn("prebind: dropping entry", "reason", reason)
			continue
		}
		id := e.MessageID()
		if senders[id] == nil {
			senders[id] = map[string]bool{}
		}
		senders[id][e.Sender] = true
		kept = append(kept, e)
	}
	for _, e := range kept {
		if len(senders[e.MessageID()]) > 1 {
			s.log.Warn("prebind: dropping entry", "reason", "message id used by several senders")
			continue
		}
		if err := imp(e); err != nil {
			return err
		}
	}
	return s.removeClaimed(claimed)
}

func (s *Store) removeClaimed(claimed string) error {
	unlock, err := s.lock(false)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(claimed); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// verify returns "" for an entry that may be replayed into t's journal, or
// the reason it may not.
func (s *Store) verify(t Target, e Entry) string {
	switch {
	case e.V != Version:
		return "version"
	case e.AgentID != t.AgentID || e.Harness != t.Harness || e.SessionRef != t.SessionRef:
		return "binding"
	case !deliveredState(e.State):
		return "state"
	case !isAgentID(e.Sender):
		return "sender"
	}
	tok, err := s.readToken(e.Sender)
	if err != nil {
		return "sender token"
	}
	got, err := hex.DecodeString(e.MAC)
	if err != nil || !hmac.Equal(got, mustHex(sign(tok, e))) {
		return "mac"
	}
	var m msg.Message
	if json.Unmarshal([]byte(e.Message), &m) != nil || m.MessageID == "" {
		return "message"
	}
	if s.now().Sub(e.DeliveredAt) > s.ttl {
		return "expired"
	}
	return ""
}

func mustHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}
