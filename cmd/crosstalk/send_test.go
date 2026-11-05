package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

type recordingReceiver struct {
	mu  sync.Mutex
	got []receiver.Inbound
}

func (r *recordingReceiver) Receive(_ context.Context, in receiver.Inbound) receiver.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, in)
	return receiver.Result{MessageID: "m1", State: adapter.Accepted}
}

type noDir struct{}

func (noDir) List() ([]registry.Descriptor, error) { return nil, nil }

func TestSendGoesThroughTheReceiverChainAsUnverified(t *testing.T) {
	d, err := os.MkdirTemp("/tmp", "xt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	rcv := &recordingReceiver{}
	s := &udsingress.Server{Path: filepath.Join(d, "a.sock"), Directory: noDir{}, Receiver: rcv, ReadTimeout: 2 * time.Second}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	ack, err := sendTo(registry.Descriptor{UDSPath: s.Path}, "nightly backup finished")
	if err != nil {
		t.Fatal(err)
	}
	if ack.State != "accepted" || ack.MessageID != "m1" {
		t.Fatalf("ack %+v", ack)
	}
	if len(rcv.got) != 1 {
		t.Fatalf("receiver got %d", len(rcv.got))
	}
	in := rcv.got[0]
	if in.Ingress != receiver.UDS || in.Sender.Verified || !strings.Contains(in.Message.Text(), "nightly backup finished") {
		t.Fatalf("inbound %+v", in)
	}
}

func TestPickTargetKeepsIDsAndNamesApart(t *testing.T) {
	id := "0123456789abcdef"
	ds := []registry.Descriptor{{AgentID: "fedcba9876543210", Name: id}, {AgentID: "1111111111111111", Name: "alice"}}
	if d, err := pickTarget(ds, id); err == nil {
		t.Fatalf("an absent id matched the session named %s: %v", id, d)
	}
	ds = append(ds, registry.Descriptor{AgentID: id, Name: "bob"})
	if d, err := pickTarget(ds, id); err != nil || d.Name != "bob" {
		t.Fatalf("live id colliding with a name: %v %v", d, err)
	}
	if d, err := pickTarget(ds, "alice"); err != nil || d.AgentID != "1111111111111111" {
		t.Fatalf("by name %v %v", d, err)
	}
}

func TestPickTarget(t *testing.T) {
	ds := []registry.Descriptor{{AgentID: "aaaaaaaaaaaaaaa1", Name: "alice"}, {AgentID: "bbbbbbbbbbbbbbb1", Name: "dup"}, {AgentID: "bbbbbbbbbbbbbbb2", Name: "dup"}}
	if d, err := pickTarget(ds, "alice"); err != nil || d.AgentID != "aaaaaaaaaaaaaaa1" {
		t.Fatalf("by name %v %v", d, err)
	}
	if d, err := pickTarget(ds, "bbbbbbbbbbbbbbb2"); err != nil || d.AgentID != "bbbbbbbbbbbbbbb2" {
		t.Fatalf("by id %v %v", d, err)
	}
	if _, err := pickTarget(ds, "dup"); err == nil {
		t.Fatal("ambiguous name accepted")
	}
	if _, err := pickTarget(ds, "ghost"); err == nil {
		t.Fatal("unknown name accepted")
	}
}

type slowReceiver struct{ recordingReceiver }

func (r *slowReceiver) Receive(ctx context.Context, in receiver.Inbound) receiver.Result {
	time.Sleep(600 * time.Millisecond)
	return r.recordingReceiver.Receive(ctx, in)
}

func TestSendTimeoutAfterWriteIsUnknownOutcome(t *testing.T) {
	d, err := os.MkdirTemp("/tmp", "xt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	s := &udsingress.Server{Path: filepath.Join(d, "a.sock"), Directory: noDir{}, Receiver: &slowReceiver{}, ReadTimeout: 2 * time.Second}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	old := sendTimeout
	sendTimeout = 100 * time.Millisecond
	defer func() { sendTimeout = old }()
	_, err = sendTo(registry.Descriptor{UDSPath: s.Path}, "x")
	if err == nil || !strings.Contains(err.Error(), "may still be delivered") {
		t.Fatalf("err %v", err)
	}
}

func TestSendTooLargeIsAClearError(t *testing.T) {
	_, err := sendTo(registry.Descriptor{UDSPath: "/nonexistent/a.sock"}, strings.Repeat("x", 3<<20))
	if err == nil || !strings.Contains(err.Error(), "limit is") {
		t.Fatalf("err %v", err)
	}
}
