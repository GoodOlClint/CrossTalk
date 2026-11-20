package codex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// WakeText is the whole of a wake row's user input: fixed, with no sender,
// count, message id or peer text.
const WakeText = "You have new CrossTalk mail. Call the receive_message tool of MCP server crosstalk to read it."

const (
	// MaxQueueItems is Codex's MAX_QUEUE_ITEMS at 0.167.0; it is applied to
	// the whole table, which is never fewer rows than one thread's.
	MaxQueueItems = 100
	// DefaultLockTimeout bounds the wait for another shim's hold on a
	// thread's record.
	DefaultLockTimeout = 2 * time.Second
	// DefaultAdmissionBound is how long a generation stays admitted (its row
	// taken, no pull yet) before it closes unknown.
	DefaultAdmissionBound = 2 * time.Minute
)

type GenState string

const (
	GenPending   GenState = "pending"
	GenQueued    GenState = "queued"
	GenAdmitted  GenState = "admitted"
	GenCollected GenState = "collected"
	GenUnknown   GenState = "unknown"
	GenFailed    GenState = "failed"
)

func (s GenState) open() bool { return s == GenPending || s == GenQueued || s == GenAdmitted }

var (
	ErrWakeRowExists = errors.New("codex queue: a CrossTalk wake row for the thread is already queued")
	ErrQueueFull     = errors.New("codex queue: at the item cap")
	ErrWakeLocked    = errors.New("codex wake: the thread's record is locked past the timeout")
)

// wakeNamespace scopes wake ids, so a wake id never equals a client id
// Codex generates.
var wakeNamespace = uuid.MustParse("5b0d6f0e-6a43-4b8e-9a51-0c2a7e1f9d34")

func wakeID(thread string, gen int) string {
	return uuid.NewSHA1(wakeNamespace, []byte(thread+"\x00"+strconv.Itoa(gen))).String()
}

// Generation is a thread's latest wake generation. Only the latest is kept:
// one generation is open at a time and a closed one is never reopened, so a
// run of failures overwrites one record and counts it in Failures.
type Generation struct {
	Thread string    `json:"thread"`
	N      int       `json:"gen"`
	WakeID string    `json:"wake_id,omitempty"`
	State  GenState  `json:"state,omitempty"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
	// Failures counts consecutive failed generations ending at this one.
	Failures int `json:"failures,omitempty"`
	// Eligible lists message ids that arrived on a waking route and may
	// still be mailboxed.
	Eligible []string `json:"eligible,omitempty"`
}

// Wakes keeps the wake generations of one shim's threads in Dir, one file
// per thread, each written before the action its state names.
type Wakes struct {
	Dir string
	// Gate returns a fresh gate for one transaction.
	Gate  func() *QueueGate
	Now   func() time.Time
	Bound time.Duration
	// LockTimeout bounds the cross-process lock wait; 0 means
	// DefaultLockTimeout. On timeout nothing is written: no record change,
	// no row.
	LockTimeout time.Duration

	mu       sync.Mutex
	threads  map[string]*sync.Mutex
	replayed map[string]bool

	crash func(point string) error
}

func (w *Wakes) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Wakes) bound() time.Duration {
	if w.Bound > 0 {
		return w.Bound
	}
	return DefaultAdmissionBound
}

// lock serializes thread's record within this process and, by a flock on
// <thread>.lock in Dir, with any other shim that loads the same thread (an
// ownership transfer).
func (w *Wakes) lock(thread string) (func(), error) {
	if !sessionIDRe.MatchString(thread) {
		return nil, ErrBadSessionID
	}
	w.mu.Lock()
	if w.threads == nil {
		w.threads = map[string]*sync.Mutex{}
	}
	mu := w.threads[thread]
	if mu == nil {
		mu = new(sync.Mutex)
		w.threads[thread] = mu
	}
	w.mu.Unlock()
	mu.Lock()
	if err := ensurePrivateDir(w.Dir); err != nil {
		mu.Unlock()
		return nil, err
	}
	timeout := w.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	unlock, err := flockFile(filepath.Join(w.Dir, thread+".lock"), timeout)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	return func() { unlock(); mu.Unlock() }, nil
}

func flockFile(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, ErrWakeLocked
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (w *Wakes) path(thread string) string { return filepath.Join(w.Dir, thread+".json") }

// Generation returns thread's persisted record.
func (w *Wakes) Generation(thread string) (Generation, error) {
	unlock, err := w.lock(thread)
	if err != nil {
		return Generation{}, err
	}
	defer unlock()
	return w.read(thread)
}

func (w *Wakes) read(thread string) (Generation, error) {
	if !sessionIDRe.MatchString(thread) {
		return Generation{}, ErrBadSessionID
	}
	b, err := os.ReadFile(w.path(thread))
	if errors.Is(err, os.ErrNotExist) {
		return Generation{Thread: thread}, nil
	}
	if err != nil {
		return Generation{}, err
	}
	var g Generation
	if err := json.Unmarshal(b, &g); err != nil || g.Thread != thread {
		return Generation{}, fmt.Errorf("codex wake: bad record for %s: %v", thread, err)
	}
	return g, nil
}

// write replaces thread's record durably: a temp file synced, renamed over
// the record, and the directory synced.
func (w *Wakes) write(g Generation) error {
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(w.Dir, "."+g.Thread+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, w.path(g.Thread)); err != nil {
		return err
	}
	d, err := os.Open(w.Dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (w *Wakes) set(g *Generation, s GenState, reason string) error {
	g.State, g.At, g.Reason = s, w.now(), reason
	return w.write(*g)
}

// load reads thread's record and, the first time this process sees the
// thread, applies the replay rules through observe.
func (w *Wakes) load(ctx context.Context, thread string) (Generation, error) {
	g, err := w.read(thread)
	if err != nil {
		return g, err
	}
	w.mu.Lock()
	done := w.replayed[thread]
	if w.replayed == nil {
		w.replayed = map[string]bool{}
	}
	w.replayed[thread] = true
	w.mu.Unlock()
	if done {
		return g, nil
	}
	return g, w.observe(ctx, &g)
}

// observe reads the queue and never writes a row. Under the thread lock a
// pending record is never the caller's own transaction in flight, so it is a
// crash or a failed outcome write: its row found is queued, not found or
// unreadable is unknown (the commit never happened, or Codex already took
// the row). A queued generation whose row is gone is admitted; an admitted
// one past the admission bound is unknown. A queue it cannot read leaves a
// queued generation as it is.
func (w *Wakes) observe(ctx context.Context, g *Generation) error {
	if g.State == GenPending {
		if present, err := w.hasRow(ctx, g.WakeID); err == nil && present {
			return w.set(g, GenQueued, "replay: row found")
		}
		return w.set(g, GenUnknown, "replay: pending, row not found")
	}
	if g.State == GenQueued {
		present, err := w.hasRow(ctx, g.WakeID)
		if err != nil || present {
			return nil
		}
		if err := w.set(g, GenAdmitted, ""); err != nil {
			return err
		}
	}
	if g.State == GenAdmitted && w.now().Sub(g.At) >= w.bound() {
		return w.set(g, GenUnknown, "no pull within the admission bound")
	}
	return nil
}

func (w *Wakes) hasRow(ctx context.Context, id string) (bool, error) {
	var n int
	err := w.Gate().Tx(ctx, false, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM queued_items WHERE CASE WHEN json_valid(payload_json) THEN json_extract(payload_json, '$.UserInput.client_id') END = ?", id).Scan(&n)
	})
	return n > 0, err
}

// Signal is MailSignal for one mailboxed entry. A wake-eligible entry is
// remembered; if the thread has no open generation and precheck passes, a
// new generation is recorded pending, its row written in one transaction,
// and the outcome recorded queued, failed or (a commit that errored)
// unknown.
func (w *Wakes) Signal(ctx context.Context, thread, messageID string, eligible bool, precheck func() error) error {
	if !eligible {
		return nil
	}
	unlock, err := w.lock(thread)
	if err != nil {
		return err
	}
	defer unlock()
	g, err := w.load(ctx, thread)
	if err != nil {
		return err
	}
	if !slices.Contains(g.Eligible, messageID) {
		g.Eligible = append(g.Eligible, messageID)
		if err := w.write(g); err != nil {
			return err
		}
	}
	return w.open(ctx, &g, precheck)
}

func (w *Wakes) open(ctx context.Context, g *Generation, precheck func() error) error {
	if err := w.observe(ctx, g); err != nil {
		return err
	}
	if g.State.open() {
		return nil
	}
	if err := precheck(); err != nil {
		return err
	}
	failures := 0
	if g.State == GenFailed {
		failures = g.Failures
	}
	g.N++
	g.WakeID, g.Failures = wakeID(g.Thread, g.N), 0
	if err := w.set(g, GenPending, ""); err != nil {
		return err
	}
	if err := w.crashAt("after-pending"); err != nil {
		return err
	}
	err := w.Gate().Tx(ctx, true, func(conn *sql.Conn) error {
		return insertWake(ctx, conn, g.Thread, g.WakeID, w.now())
	})
	switch {
	case errors.Is(err, ErrQueueAmbiguous):
		return w.set(g, GenUnknown, err.Error())
	case err != nil:
		g.Failures = failures + 1
		return w.set(g, GenFailed, err.Error())
	}
	if err := w.crashAt("after-commit"); err != nil {
		return err
	}
	return w.set(g, GenQueued, "")
}

func (w *Wakes) crashAt(point string) error {
	if w.crash != nil {
		return w.crash(point)
	}
	return nil
}

// Pulled records a top-level pull from thread: an open generation closes
// collected, and the remembered wake-eligible ids are pruned to those still
// mailboxed. When a generation closed and more is false, a wake-eligible
// entry still mailboxed (one that arrived after the pull's snapshot, while
// the old generation was open) opens the next generation.
func (w *Wakes) Pulled(ctx context.Context, thread string, more bool, mailboxed func() ([]string, error), precheck func() error) error {
	unlock, err := w.lock(thread)
	if err != nil {
		return err
	}
	defer unlock()
	g, err := w.load(ctx, thread)
	if err != nil {
		return err
	}
	if err := w.observe(ctx, &g); err != nil {
		return err
	}
	closed := g.State == GenQueued || g.State == GenAdmitted
	if closed {
		if err := w.set(&g, GenCollected, ""); err != nil {
			return err
		}
	}
	if len(g.Eligible) > 0 {
		ids, err := mailboxed()
		if err != nil {
			return err
		}
		kept := g.Eligible[:0:0]
		for _, id := range g.Eligible {
			if slices.Contains(ids, id) {
				kept = append(kept, id)
			}
		}
		if len(kept) != len(g.Eligible) {
			g.Eligible = kept
			if err := w.write(g); err != nil {
				return err
			}
		}
	}
	if !closed || more || len(g.Eligible) == 0 {
		return nil
	}
	return w.open(ctx, &g, precheck)
}

// Tick observes every thread this process has loaded, and keep still
// accepts, with an open generation. It never writes a row.
func (w *Wakes) Tick(ctx context.Context, keep func(thread string) bool) {
	w.mu.Lock()
	var threads []string
	for t := range w.replayed {
		threads = append(threads, t)
	}
	w.mu.Unlock()
	for _, t := range threads {
		if keep != nil && !keep(t) {
			continue
		}
		unlock, err := w.lock(t)
		if err != nil {
			continue
		}
		if g, err := w.read(t); err == nil && g.State.open() {
			w.observe(ctx, &g)
		}
		unlock()
	}
}

type wakeInput struct {
	UserInput wakeUserInput `json:"UserInput"`
}

type wakeUserInput struct {
	Content  []wakeContent `json:"content"`
	ClientID string        `json:"client_id"`
}

type wakeContent struct {
	Type         string   `json:"type"`
	Text         string   `json:"text"`
	TextElements []string `json:"text_elements"`
}

// wakePayload is the row JSON in the shape `codex queue` writes at 0.167.0.
func wakePayload(id string) string {
	b, _ := json.Marshal(wakeInput{UserInput: wakeUserInput{Content: []wakeContent{{Type: "text", Text: WakeText, TextElements: []string{}}}, ClientID: id}})
	return string(b)
}

// insertWake writes one wake row for thread at the end of its queue, unless
// a CrossTalk wake row for it is already queued or the table is at the cap.
func insertWake(ctx context.Context, conn *sql.Conn, thread, id string, now time.Time) error {
	var total, mine int
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(thread_id = ? AND CASE WHEN json_valid(payload_json) THEN json_extract(payload_json, '$.UserInput.content[0].text') END = ?), 0) FROM queued_items`, thread, WakeText).Scan(&total, &mine)
	if err != nil {
		return queueErr(err)
	}
	if mine > 0 {
		return ErrWakeRowExists
	}
	if total >= MaxQueueItems {
		return ErrQueueFull
	}
	ms := now.UnixMilli()
	_, err = conn.ExecContext(ctx, `INSERT INTO queued_items (id, thread_id, payload_json, queue_order, created_at_ms, updated_at_ms)
VALUES (?, ?, ?, (SELECT COALESCE(MAX(queue_order) + 1, 0) FROM queued_items WHERE thread_id = ?), ?, ?)`, uuid.NewString(), thread, wakePayload(id), thread, ms, ms)
	if err != nil {
		return queueErr(err)
	}
	return nil
}
