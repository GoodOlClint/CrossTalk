package pi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// answerTimeout is D7's answer deadline; a frame is never re-sent.
var answerTimeout = 10 * time.Second

// frame is the shim->extension delivery line ext/index.ts reads.
type frame struct {
	ID      string `json:"id"`
	Deliver string `json:"deliver"`
	Trigger bool   `json:"trigger"`
	Text    string `json:"text"`
}

// waiter receives true for {ok:true}, false for any other answer, and is
// closed when its connection drops. An answer at or after deadline (zero
// until the frame is written) is dropped.
type waiter struct {
	conn     net.Conn
	ch       chan bool
	deadline time.Time
}

// rawSender is D6 (ADR 0037 point 4): only a verified registered agent of
// this host gets its text into the session; every other class is mailboxed.
func rawSender(d adapter.Delivery) bool {
	return d.VerifiedSender && !d.SenderRemote && !d.InboxCopy &&
		(d.Notice == msg.NoticeTeammate || d.Notice == msg.NoticeTeammateAgain)
}

// Inject writes one frame and maps its one answer to a state (D7). Mode
// alone picks the trigger; the message kind is not an input.
func (h *Harness) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	if !rawSender(d) {
		return adapter.Mailboxed, nil
	}
	if err := h.served(d.Target); err != nil {
		return adapter.Unreachable, err
	}
	mode := d.Mode
	if mode == "" {
		mode = m.Delivery
	}
	text := RenderIn(m, d.VerifiedSender, d.SenderHarness, d.SenderAgentID, d.Inbox, d.InboxCopy, d.Notice)
	return h.deliver(ctx, text, mode != msg.NextTurn, true)
}

func (h *Harness) served(target adapter.Binding) error {
	if p, ok := h.session(); !ok || p.ref() != target.SessionRef {
		return fmt.Errorf("pi: deliver %s: not the session this shim serves; nothing sent", target.SessionRef)
	}
	return nil
}

func (h *Harness) deliver(ctx context.Context, text string, trigger, wait bool) (adapter.State, error) {
	id := rand.Text()
	h.mu.Lock()
	c := h.conn
	if c == nil {
		h.mu.Unlock()
		return adapter.Unreachable, errors.New("pi: deliver: the extension is not connected; nothing sent")
	}
	var ch chan bool
	if wait {
		ch = make(chan bool, 1)
		h.waiters[id] = waiter{conn: c, ch: ch}
	}
	h.mu.Unlock()
	if ctx.Err() != nil {
		h.unwait(id)
		return adapter.Unreachable, errors.New("pi: deliver: canceled before send; nothing sent")
	}
	b, _ := json.Marshal(frame{ID: id, Deliver: "steer", Trigger: trigger, Text: text})
	if err := h.write(c, append(b, '\n')); err != nil {
		h.unwait(id)
		if errors.Is(err, errWriteBusy) {
			return adapter.Unreachable, fmt.Errorf("pi: deliver: %w; nothing sent", err)
		}
		return adapter.Unknown, fmt.Errorf("pi: deliver: write: %w", err)
	}
	if !wait {
		return adapter.Accepted, nil
	}
	deadline := time.Now().Add(answerTimeout)
	h.mu.Lock()
	if w, ok := h.waiters[id]; ok {
		w.deadline = deadline
		h.waiters[id] = w
	}
	h.mu.Unlock()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case ok, open := <-ch:
		switch {
		case !open:
			return adapter.Unknown, errors.New("pi: deliver: connection dropped before an answer")
		case !ok:
			return adapter.Unknown, errors.New("pi: deliver: the extension did not confirm admission")
		case trigger:
			return adapter.Accepted, nil
		}
		return adapter.Queued, nil
	case <-t.C:
		h.unwait(id)
		return adapter.Unknown, fmt.Errorf("pi: deliver: no admission within %v", answerTimeout)
	case <-ctx.Done():
		h.unwait(id)
		return adapter.Unknown, fmt.Errorf("pi: deliver: %w after send", ctx.Err())
	}
}

func (h *Harness) unwait(id string) {
	h.mu.Lock()
	delete(h.waiters, id)
	h.mu.Unlock()
}

// answer settles the waiter for a {id, ok} line from c. An answer with no
// waiter (late, after the deadline, or for a wake) changes nothing.
func (h *Harness) answer(c net.Conn, f map[string]json.RawMessage) {
	var id string
	if json.Unmarshal(f["id"], &id) != nil {
		h.logger.Warn("pi: malformed answer from the extension")
		return
	}
	var ok bool
	good := json.Unmarshal(f["ok"], &ok) == nil
	h.mu.Lock()
	w, found := h.waiters[id]
	found = found && w.conn == c && (w.deadline.IsZero() || time.Now().Before(w.deadline))
	if found {
		delete(h.waiters, id)
	}
	h.mu.Unlock()
	if !found {
		h.logger.Debug("pi: answer with no waiting delivery dropped", "id", id)
		return
	}
	w.ch <- good && ok
}
