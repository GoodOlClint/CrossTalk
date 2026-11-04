package a2a

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

type failNthSave struct {
	*MemoryTaskStore
	id    atomic.Value
	fail  int32
	saves atomic.Int32
}

func (s *failNthSave) Save(r journal.Record) error {
	if id, _ := s.id.Load().(string); id != "" && bytes.Contains(r.Message, []byte(id)) && s.saves.Add(1) == s.fail {
		return errors.New("continuation history write failed")
	}
	return s.MemoryTaskStore.Save(r)
}

func openAsk(t *testing.T, e *env) (*sdk.Task, *sdk.Message) {
	t.Helper()
	cl, ctx := e.peer()
	first, err := send(t, cl, ctx, textMsg("first?", map[string]any{"kind": "ask"}), true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, cl, ctx, first.ID, sdk.TaskStateWorking)
	second := textMsg("revised?", map[string]any{"kind": "ask"})
	second.TaskID, second.ContextID = first.ID, first.ContextID
	return first, second
}

func continuationEntries(h []*sdk.Message, id string) []string {
	var states []string
	for _, m := range h {
		if m.ID == id {
			states = append(states, deliveryState(m))
		}
	}
	return states
}

func TestReplyFromContinuationInjectionCompletesTask(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cl, ctx := e.peer()
	first, second := openAsk(t, e)
	replyErr := make(chan error, 1)
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Accepted, After: func(adapter.Call) { replyErr <- e.srv.CompleteAsk(second.ID, "answer") }})
	if _, err := send(t, cl, ctx, second, true); err != nil {
		t.Fatal(err)
	}
	if err := <-replyErr; err != nil {
		t.Fatalf("reply to the continuation being injected: %v", err)
	}
	if got := waitState(t, cl, ctx, first.ID, sdk.TaskStateCompleted); messageText(got.Status.Message) != "answer" {
		t.Fatalf("task status %+v", got.Status)
	}
}

func TestContinuationRecordFailureBeforeDelivery(t *testing.T) {
	ts := &failNthSave{MemoryTaskStore: NewMemoryTaskStore(), fail: 1}
	e := newEnv(t, receiver.Limits{}, Limits{}, func(s *Server) { s.tasks.ts = ts })
	cl, ctx := e.peer()
	_, second := openAsk(t, e)
	ts.id.Store(second.ID)
	if _, err := send(t, cl, ctx, second, true); err == nil {
		t.Fatal("failed association write acknowledged")
	}
	if n := len(e.fake.Writes()); n != 1 {
		t.Fatalf("continuation delivered without its task association: %d writes", n)
	}
	if _, _, known := e.srv.AskOpen(second.ID); known {
		t.Fatal("handle indexed after its write failed")
	}
	if _, err := send(t, cl, ctx, second, true); err != nil {
		t.Fatal(err)
	}
	if _, _, known := e.srv.AskOpen(second.ID); !known || len(e.fake.Writes()) != 2 {
		t.Fatalf("retry: known=%t writes=%d", known, len(e.fake.Writes()))
	}
}

func TestContinuationHistoryFailureAfterInjectionReconciledOnRetry(t *testing.T) {
	ts := &failNthSave{MemoryTaskStore: NewMemoryTaskStore(), fail: 2}
	e := newEnv(t, receiver.Limits{}, Limits{}, func(s *Server) { s.tasks.ts = ts })
	cl, ctx := e.peer()
	first, second := openAsk(t, e)
	ts.id.Store(second.ID)
	if _, err := send(t, cl, ctx, second, true); err == nil {
		t.Fatal("failed history write acknowledged")
	}
	if n := len(e.fake.Writes()); n != 2 {
		t.Fatalf("writes %d, want the ask and one continuation", n)
	}
	if _, _, known := e.srv.AskOpen(second.ID); !known {
		t.Fatal("delivered continuation has no task association")
	}
	got, err := send(t, cl, ctx, second, true)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := len(e.fake.Writes()); n != 2 {
		t.Fatalf("retry re-injected: %d writes", n)
	}
	if id, _, known := e.srv.AskOpen(second.ID); !known || id != string(first.ID) {
		t.Fatalf("AskOpen(continuation) id=%q known=%t", id, known)
	}
	if s := continuationEntries(got.History, second.ID); !slices.Equal(s, []string{"accepted"}) {
		t.Fatalf("continuation history states %v", s)
	}
	replayed := newStore(ts, e.srv.tasks.log)
	if id, _, known := replayed.askOpen(second.ID); !known || id != string(first.ID) {
		t.Fatalf("replayed AskOpen(continuation) id=%q known=%t", id, known)
	}
	rt, _ := replayed.byMessage(second.ID)
	if s := continuationEntries(replayed.snapshot(rt, nil).History, second.ID); !slices.Equal(s, []string{"accepted"}) {
		t.Fatalf("replayed continuation history states %v", s)
	}
}

func TestRejectedContinuationLeavesNoHandle(t *testing.T) {
	e := newEnv(t, receiver.Limits{MaxMessageBytes: 8}, Limits{}, nil)
	cl, ctx := e.peer()
	first, _ := openAsk(t, e)
	long := textMsg("far too long for the limit", map[string]any{"kind": "ask"})
	long.TaskID, long.ContextID = first.ID, first.ContextID
	if _, err := send(t, cl, ctx, long, true); err == nil {
		t.Fatal("oversize continuation accepted")
	}
	if _, _, known := e.srv.AskOpen(long.ID); known {
		t.Fatal("rejected continuation is a handle")
	}
	got, err := cl.GetTask(ctx, &sdk.GetTaskRequest{ID: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 {
		t.Fatalf("history %d entries after a rejected continuation", len(got.History))
	}
	replayed := newStore(e.srv.tasks.ts, e.srv.tasks.log)
	if _, _, known := replayed.askOpen(long.ID); known {
		t.Fatal("rejected continuation is a handle after replay")
	}
	rt, _ := replayed.byMessage(meta(first)["message_id"].(string))
	if n := len(replayed.snapshot(rt, nil).History); n != 1 {
		t.Fatalf("replayed history %d entries", n)
	}
}

type terminatingReceiver struct {
	Receiver
	before func(receiver.Inbound)
	after  func(receiver.Inbound)
}

func (r terminatingReceiver) Receive(ctx context.Context, in receiver.Inbound) receiver.Result {
	r.before(in)
	res := r.Receiver.Receive(ctx, in)
	r.after(in)
	return res
}

func TestTaskTerminalDuringContinuationReceiveClosesItsRoute(t *testing.T) {
	var (
		mu       sync.Mutex
		received bool
		closed   []string
		secondID string
		firstID  string
	)
	e := newEnv(t, receiver.Limits{}, Limits{}, func(s *Server) {
		s.cfg.OnAskClosed = func(ids []string) {
			mu.Lock()
			defer mu.Unlock()
			if received {
				closed = append(closed, ids...)
			}
		}
		s.cfg.Receiver = terminatingReceiver{
			Receiver: s.cfg.Receiver,
			before: func(in receiver.Inbound) {
				mu.Lock()
				match, ask := in.Message.MessageID == secondID, firstID
				mu.Unlock()
				if match {
					if err := s.CompleteAsk(ask, "done"); err != nil {
						t.Error(err)
					}
				}
			},
			after: func(in receiver.Inbound) {
				mu.Lock()
				if in.Message.MessageID == secondID {
					received = true
				}
				mu.Unlock()
			},
		}
	})
	cl, ctx := e.peer()
	first, second := openAsk(t, e)
	mu.Lock()
	firstID, secondID = meta(first)["message_id"].(string), second.ID
	mu.Unlock()
	send(t, cl, ctx, second, true)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(closed, second.ID) {
		t.Fatalf("routes closed after the continuation was received: %v", closed)
	}
}

func TestAgentRoleContinuationRejectedBeforeDelivery(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	cl, ctx := e.peer()
	_, second := openAsk(t, e)
	second.Role = sdk.MessageRoleAgent
	_, err := send(t, cl, ctx, second, true)
	if err == nil {
		t.Fatal("agent-role continuation was accepted")
	}
	if st := errorState(err); st != adapter.Rejected {
		t.Fatalf("agent-role continuation mapped to %q, want rejected: %v", st, err)
	}
	if _, _, open := e.srv.AskOpen(second.ID); open {
		t.Fatal("rejected continuation was indexed as an ask handle")
	}
	replayed := newStore(e.srv.tasks.ts, e.srv.tasks.log)
	if _, _, open := replayed.askOpen(second.ID); open {
		t.Fatal("rejected continuation resolves after replay")
	}
}
