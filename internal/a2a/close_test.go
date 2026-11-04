package a2a

import (
	"context"
	"errors"
	"testing"
	"time"

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
