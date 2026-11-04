package a2a

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

type stuckReceiver struct{ entered, release chan struct{} }

func (r *stuckReceiver) Receive(_ context.Context, in receiver.Inbound) receiver.Result {
	close(r.entered)
	<-r.release
	return receiver.Result{MessageID: in.Message.MessageID}
}

func (r *stuckReceiver) Cancel(string) (bool, error) { return false, nil }

func TestCloseContextBoundedWithStuckReceiver(t *testing.T) {
	ts, _ := submittedOnly(t, msg.Inform)
	r := &stuckReceiver{entered: make(chan struct{}), release: make(chan struct{})}
	s := NewServer(Config{Receiver: r, Tasks: ts})
	s.Resume()
	<-r.entered

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.CloseContext(ctx) }()
	select {
	case err := <-done:
		if el := time.Since(start); el > 150*time.Millisecond {
			t.Fatalf("CloseContext took %v with a 30ms deadline", el)
		}
		if !errors.Is(err, ErrWorkersRunning) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ErrWorkersRunning wrapping the deadline", err)
		}
	case <-time.After(150 * time.Millisecond):
		close(r.release)
		<-done
		t.Fatal("CloseContext still blocked on a receiver that ignores cancellation")
	}
	if s.base.Err() == nil {
		t.Fatal("receive context not cancelled")
	}
	close(r.release)
	if err := s.Close(); err != nil {
		t.Fatalf("Close after workers drained: %v", err)
	}
}

type gatedContinuation struct {
	Receiver
	id               string
	entered, release chan struct{}
	mu               sync.Mutex
	seen             []string
}

func (r *gatedContinuation) Receive(ctx context.Context, in receiver.Inbound) receiver.Result {
	r.mu.Lock()
	r.seen = append(r.seen, in.Message.MessageID)
	gate := in.Message.MessageID == r.id && r.entered != nil
	r.mu.Unlock()
	if gate {
		close(r.entered)
		<-r.release
	}
	return r.Receiver.Receive(ctx, in)
}

func (r *gatedContinuation) received(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.seen {
		if s == id {
			return true
		}
	}
	return false
}

func sendContinuationDirect(s *Server, m *sdk.Message) error {
	h := &handler{s: s}
	ctx := context.WithValue(context.Background(), senderKey{}, receiver.Sender{Verified: true, Principal: receiver.PrincipalLocalUser})
	_, err := h.SendMessage(ctx, &sdk.SendMessageRequest{Message: m, Config: &sdk.SendMessageConfig{ReturnImmediately: true}})
	return err
}

func TestCloseContextWaitsForContinuationReceive(t *testing.T) {
	r := &gatedContinuation{entered: make(chan struct{}), release: make(chan struct{})}
	e := newEnv(t, receiver.Limits{}, Limits{}, func(s *Server) { r.Receiver, s.cfg.Receiver = s.cfg.Receiver, r })
	_, second := openAsk(t, e)
	r.mu.Lock()
	r.id = second.ID
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		sendContinuationDirect(e.srv, second)
		close(done)
	}()
	<-r.entered

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := e.srv.CloseContext(ctx); !errors.Is(err, ErrWorkersRunning) {
		close(r.release)
		<-done
		t.Fatalf("CloseContext with a continuation receive running = %v, want ErrWorkersRunning", err)
	}
	close(r.release)
	<-done
	if err := e.srv.Close(); err != nil {
		t.Fatalf("Close after the continuation returned: %v", err)
	}
}

func TestContinuationAfterCloseRefused(t *testing.T) {
	r := &gatedContinuation{}
	e := newEnv(t, receiver.Limits{}, Limits{}, func(s *Server) { r.Receiver, s.cfg.Receiver = s.cfg.Receiver, r })
	_, second := openAsk(t, e)
	if err := e.srv.Close(); err != nil {
		t.Fatal(err)
	}
	err := sendContinuationDirect(e.srv, second)
	if !errors.Is(err, sdk.ErrServerError) {
		t.Fatalf("continuation after close = %v, want server closing error", err)
	}
	if r.received(second.ID) {
		t.Fatal("continuation after close reached the receiver")
	}
}
