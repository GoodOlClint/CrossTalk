package a2a

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

type countingReceiver struct {
	calls  atomic.Int32
	result receiver.Result
}

func (r *countingReceiver) Receive(_ context.Context, in receiver.Inbound) receiver.Result {
	r.calls.Add(1)
	res := r.result
	res.MessageID = in.Message.MessageID
	if res.State == "" {
		res.State = adapter.Accepted
	}
	return res
}

func (*countingReceiver) Cancel(string) (bool, error) { return true, nil }

func submittedOnly(t *testing.T, kind msg.Kind) (*MemoryTaskStore, msg.Message) {
	t.Helper()
	ts := NewMemoryTaskStore()
	m := msg.Message{MessageID: msg.NewUUIDv7(), ContextID: "ctx", Kind: kind, Parts: []msg.Part{{Type: "text", Text: "hello"}}}
	if _, _, err := newStore(ts, slog.New(slog.DiscardHandler)).create(receiver.PrincipalLocalUser, m, encode(m), 32); err != nil {
		t.Fatal(err)
	}
	return ts, m
}

func TestSubmittedTaskResumesAfterRestart(t *testing.T) {
	ts, m := submittedOnly(t, msg.Inform)
	r := &countingReceiver{}
	s := NewServer(Config{Tasks: ts, Receiver: r})
	defer s.Close()
	s.Resume()

	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), senderKey{}, receiver.Sender{Verified: true, Principal: receiver.PrincipalLocalUser}), 2*time.Second)
	defer cancel()
	got, err := (&handler{s: s}).SendMessage(ctx, &sdk.SendMessageRequest{Message: encode(m)})
	if err != nil {
		t.Fatalf("same-ID retry after restart: err=%v receiver_calls=%d pending=%v", err, r.calls.Load(), s.PendingMessageIDs())
	}
	if st := got.(*sdk.Task).Status.State; st != sdk.TaskStateCompleted {
		t.Fatalf("state %v, want completed", st)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("receiver called %d times, want 1", n)
	}
	if p := s.PendingMessageIDs(); len(p) != 0 {
		t.Fatalf("still pending: %v", p)
	}
}

func TestResumeAskLeavesSubmitted(t *testing.T) {
	ts, m := submittedOnly(t, msg.Ask)
	s := NewServer(Config{Tasks: ts, Receiver: &countingReceiver{}})
	defer s.Close()
	s.Resume()
	deadline := time.Now().Add(2 * time.Second)
	for {
		tk, _ := s.tasks.byMessage(m.MessageID)
		if s.tasks.snapshot(tk, nil).Status.State == sdk.TaskStateWorking {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ask stayed %v", s.tasks.snapshot(tk, nil).Status.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResumeUndecodableTaskIsUnknownJournal(t *testing.T) {
	ts := NewMemoryTaskStore()
	st := newStore(ts, slog.New(slog.DiscardHandler))
	m := msg.Message{MessageID: msg.NewUUIDv7(), ContextID: "ctx", Kind: msg.Inform}
	bad := &sdk.Message{ID: m.MessageID, ContextID: "ctx", Role: sdk.MessageRoleUser}
	if _, _, err := st.create(receiver.PrincipalLocalUser, m, bad, 32); err != nil {
		t.Fatal(err)
	}
	r := &countingReceiver{}
	s := NewServer(Config{Tasks: ts, Receiver: r})
	defer s.Close()
	s.Resume()
	tk, _ := s.tasks.byMessage(m.MessageID)
	got := s.tasks.snapshot(tk, nil)
	if got.Status.State != sdk.TaskStateWorking || taskReason(got) != receiver.ReasonJournal || r.calls.Load() != 0 {
		t.Fatalf("state %v reason %q calls %d", got.Status.State, taskReason(got), r.calls.Load())
	}
}
