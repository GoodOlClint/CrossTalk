package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

type holdingReceiver struct {
	mu   sync.Mutex
	seen []receiver.Inbound
	hold bool
}

func (r *holdingReceiver) Receive(ctx context.Context, in receiver.Inbound) receiver.Result {
	r.mu.Lock()
	r.seen = append(r.seen, in)
	hold := r.hold
	r.mu.Unlock()
	if hold {
		<-ctx.Done()
		return receiver.Result{MessageID: in.Message.MessageID, State: adapter.Unknown, Reason: receiver.ReasonJournal}
	}
	return receiver.Result{MessageID: in.Message.MessageID, State: adapter.Accepted}
}

func (*holdingReceiver) Cancel(string) (bool, error) { return true, nil }

func (r *holdingReceiver) inbounds() []receiver.Inbound {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receiver.Inbound(nil), r.seen...)
}

func localCtx() context.Context {
	return context.WithValue(context.Background(), senderKey{}, receiver.Sender{Verified: true, Principal: receiver.PrincipalLocalUser})
}

func copyMsg(kind msg.Kind, extra map[string]any) *sdk.Message {
	md := map[string]any{"kind": string(kind), "inbox": "coord", "inbox_copy": true}
	for k, v := range extra {
		md[k] = v
	}
	return textMsg("copy", md)
}

func sendImmediate(s *Server, m *sdk.Message) (*sdk.Task, error) {
	res, err := (&handler{s: s}).SendMessage(localCtx(), &sdk.SendMessageRequest{Message: m, Config: &sdk.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		return nil, err
	}
	return res.(*sdk.Task), nil
}

func TestInboxMetadataMapsToInbound(t *testing.T) {
	r := &holdingReceiver{}
	s := NewServer(Config{Receiver: r})
	defer s.Close()
	for _, c := range []struct {
		name       string
		extra      map[string]any
		inbox      string
		copied     bool
		kind       msg.Kind
		badRequest bool
	}{
		{name: "copy", extra: nil, inbox: "coord", copied: true, kind: msg.Ask},
		{name: "responder leg", extra: map[string]any{"inbox_copy": false}, inbox: "coord", kind: msg.Ask},
		{name: "flag without inbox", extra: map[string]any{"inbox": nil}, badRequest: true},
		{name: "bad inbox name", extra: map[string]any{"inbox": "a@b"}, badRequest: true},
		{name: "flag not a bool", extra: map[string]any{"inbox_copy": "yes"}, badRequest: true},
		{name: "inbox not a string", extra: map[string]any{"inbox": 7}, badRequest: true},
	} {
		before := len(r.inbounds())
		_, err := sendImmediate(s, copyMsg(msg.Ask, c.extra))
		if c.badRequest {
			if !errors.Is(err, sdk.ErrInvalidParams) {
				t.Errorf("%s: err %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for len(r.inbounds()) == before && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		in := r.inbounds()[before]
		if in.Inbox != c.inbox || in.InboxCopy != c.copied || in.Message.Kind != c.kind {
			t.Errorf("%s: inbound inbox=%q copy=%v kind=%s", c.name, in.Inbox, in.InboxCopy, in.Message.Kind)
		}
	}
}

func TestInboxAskCopiesNeverFillOpenAsks(t *testing.T) {
	ts := NewMemoryTaskStore()
	hold := &holdingReceiver{hold: true}
	lim := Limits{MaxOpenAsks: 2}
	s := NewServer(Config{Tasks: ts, Receiver: hold, Limits: lim})
	var ids []string
	for i := 0; i < 5; i++ {
		tk, err := sendImmediate(s, copyMsg(msg.Ask, map[string]any{"deadline_seconds": float64(1)}))
		if err != nil {
			t.Fatalf("ask copy %d: %v", i, err)
		}
		ids = append(ids, meta(tk)["message_id"].(string))
		if c := meta(tk)["kind"]; c != string(msg.Ask) {
			t.Fatalf("copy shows kind %v", c)
		}
	}
	for _, id := range ids {
		if tk, _ := s.tasks.byMessage(id); !tk.deadline.IsZero() {
			t.Fatal("ask copy armed a deadline")
		}
	}
	s.Close()

	direct := &holdingReceiver{}
	s2 := NewServer(Config{Tasks: ts, Receiver: direct, Limits: lim})
	defer s2.Close()
	if n := len(s2.PendingMessageIDs()); n != 5 {
		t.Fatalf("replayed pending copies %d, want 5", n)
	}
	for i := 0; i < 2; i++ {
		if _, err := sendImmediate(s2, textMsg("direct", map[string]any{"kind": "ask"})); err != nil {
			t.Fatalf("direct ask %d after restart: %v", i, err)
		}
	}
	if _, err := sendImmediate(s2, textMsg("direct", map[string]any{"kind": "ask"})); !errors.Is(err, sdk.ErrServerError) {
		t.Fatalf("third direct ask admitted: %v", err)
	}
	for _, id := range ids {
		if err := s2.CompleteAsk(id, "x"); !errors.Is(err, ErrNoSuchAsk) {
			t.Fatalf("copy is an ask task: %v", err)
		}
		if _, _, known := s2.AskOpen(id); known {
			t.Fatal("copy is a known ask")
		}
	}
}

func TestAskCopyCompletesOnDelivery(t *testing.T) {
	s := NewServer(Config{Receiver: &holdingReceiver{}})
	defer s.Close()
	res, err := (&handler{s: s}).SendMessage(localCtx(), &sdk.SendMessageRequest{Message: copyMsg(msg.Ask, nil)})
	if err != nil {
		t.Fatal(err)
	}
	tk := res.(*sdk.Task)
	if tk.Status.State != sdk.TaskStateCompleted || meta(tk)["state"] != string(adapter.Accepted) {
		t.Fatalf("ask copy task %v %v", tk.Status.State, meta(tk))
	}
}

func TestPendingCopyPersistedAsInformForOlderReaders(t *testing.T) {
	ts := NewMemoryTaskStore()
	hold := &holdingReceiver{hold: true}
	s := NewServer(Config{Tasks: ts, Receiver: hold})
	if _, err := sendImmediate(s, copyMsg(msg.Ask, nil)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	var rec journal.Record
	for _, r := range ts.Load() {
		if r.Type == journal.TypeTask {
			rec = r
			break
		}
	}
	if rec.Kind != string(msg.Inform) || rec.OrigKind != string(msg.Ask) || !rec.Copy || rec.Inbox != "coord" {
		t.Fatalf("task record %+v", rec)
	}
	var wire sdk.Message
	if err := json.Unmarshal(rec.Message, &wire); err != nil {
		t.Fatal(err)
	}
	old, err := decode(&wire)
	if err != nil || old.Kind != msg.Inform {
		t.Fatalf("a reader that ignores the markers decodes %v, %v", old.Kind, err)
	}

	submitted := NewMemoryTaskStore()
	submitted.Save(rec)
	next := &holdingReceiver{}
	s2 := NewServer(Config{Tasks: submitted, Receiver: next})
	defer s2.Close()
	s2.Resume()
	deadline := time.Now().Add(2 * time.Second)
	for len(next.inbounds()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	got := next.inbounds()
	if len(got) != 1 || got[0].Message.Kind != msg.Ask || !got[0].InboxCopy || got[0].Inbox != "coord" {
		t.Fatalf("resumed inbound %+v", got)
	}
}

func TestForgedOrigKindOnTheWireIsIgnored(t *testing.T) {
	ts := NewMemoryTaskStore()
	s := NewServer(Config{Tasks: ts, Receiver: &holdingReceiver{hold: true}})
	if _, err := sendImmediate(s, copyMsg(msg.Inform, map[string]any{"orig_kind": "command"})); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, r := range ts.Load() {
		if r.Type == journal.TypeTask && r.OrigKind != string(msg.Inform) {
			t.Fatalf("wire-supplied orig_kind reached the record: %+v", r)
		}
	}
}
