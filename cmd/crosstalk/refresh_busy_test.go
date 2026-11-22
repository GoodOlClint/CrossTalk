package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

type noIngress struct{ paused, unpaused int }

func (n *noIngress) PauseIngress() func() { n.paused++; return func() { n.unpaused++ } }

// pipeConn is a Conn over an OS pipe, which supports the read deadline the
// pause needs.
func pipeConn(t *testing.T) (*mcpserver.Conn, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	return mcpserver.NewConn(r, discard{}), w
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }

func quiesceWithin(conn *mcpserver.Conn, ing *noIngress, d time.Duration) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return quiesceForExec(ctx, ing, conn)
}

// mustWait checks that quiesce gives up while busy and undoes its pause, then
// succeeds once release has run.
func mustWait(t *testing.T, conn *mcpserver.Conn, release func()) {
	t.Helper()
	ing := &noIngress{}
	if undo, err := quiesceWithin(conn, ing, 300*time.Millisecond); err == nil {
		undo()
		t.Fatal("quiescent while busy")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("quiesce: %v", err)
	}
	if ing.unpaused != ing.paused {
		t.Fatalf("gave up with ingress paused %d, reopened %d", ing.paused, ing.unpaused)
	}
	release()
	undo, err := quiesceWithin(conn, ing, 5*time.Second)
	if err != nil {
		t.Fatalf("not quiescent after the work ended: %v", err)
	}
	undo()
}

func TestRefreshWaitsWhileBusy(t *testing.T) {
	t.Run("in-flight call", func(t *testing.T) {
		conn, w := pipeConn(t)
		w.WriteString(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
		if _, err := conn.Read(context.Background()); err != nil {
			t.Fatal(err)
		}
		id, _ := jsonrpc.MakeID(float64(1))
		mustWait(t, conn, func() {
			if err := conn.Write(context.Background(), &jsonrpc.Response{ID: id, Result: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("partial frame", func(t *testing.T) {
		conn, w := pipeConn(t)
		got := make(chan error, 1)
		go func() { _, err := conn.Read(context.Background()); got <- err }()
		frame := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
		w.WriteString(frame[:12])
		time.Sleep(50 * time.Millisecond)
		mustWait(t, conn, func() {
			w.WriteString(frame[12:] + "\n")
			if err := <-got; err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("running inject", func(t *testing.T) {
		conn, _ := pipeConn(t)
		dir, err := os.MkdirTemp("", "ctb")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		rcv := &blockingReceiver{in: make(chan struct{}), out: make(chan struct{})}
		s := &udsingress.Server{Path: filepath.Join(dir, "a.sock"), Directory: noDescriptors{}, Receiver: rcv, ReadTimeout: 5 * time.Second}
		if err := s.Listen(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.Serve(ctx)
		tok, _ := udsingress.EnsureKey(s.KeyPath)
		c, err := net.Dial("unix", s.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		auth, _ := json.Marshal(map[string]string{"type": "auth", "token": tok})
		user, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "hi"}})
		c.Write(append(append(auth, '\n'), append(user, '\n')...))
		<-rcv.in
		mustWait(t, conn, func() {
			close(rcv.out)
			bufio.NewReader(c).ReadString('\n')
		})
	})

	t.Run("running claim proof", func(t *testing.T) {
		conn, _ := pipeConn(t)
		sys := &blockingSys{in: make(chan struct{}), out: make(chan struct{})}
		cl := codex.NewClaims(codex.ClaimOptions{Sys: sys, Parent: ownerproof.Proc{PID: 1}, LockDir: t.TempDir(), Tries: 1})
		done := make(chan struct{})
		go func() {
			cl.AcceptDurable(context.Background(), codex.Claim{SessionID: "019a0000-0000-7000-8000-000000000001"})
			close(done)
		}()
		<-sys.in
		mustWait(t, conn, func() { close(sys.out); <-done })
	})

	t.Run("open wake transaction", func(t *testing.T) {
		conn, _ := pipeConn(t)
		w := &codex.Wakes{Dir: filepath.Join(t.TempDir(), "wakes")}
		in, out, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			done <- w.Signal(context.Background(), "019a0000-0000-7000-8000-000000000002", "m1", true, func() error {
				close(in)
				<-out
				return errors.New("stop before the queue")
			})
		}()
		select {
		case <-in:
		case err := <-done:
			t.Fatalf("Signal returned before its precheck: %v", err)
		}
		mustWait(t, conn, func() { close(out); <-done })
	})
}

type noDescriptors struct{}

func (noDescriptors) List() ([]registry.Descriptor, error) { return nil, nil }

type blockingReceiver struct{ in, out chan struct{} }

func (b *blockingReceiver) Receive(context.Context, receiver.Inbound) receiver.Result {
	close(b.in)
	<-b.out
	return receiver.Result{State: "accepted"}
}

// blockingSys blocks the proof's first kernel read until out is closed.
type blockingSys struct{ in, out chan struct{} }

func (b *blockingSys) Info(pid int) (ownerproof.Proc, error) {
	close(b.in)
	<-b.out
	return ownerproof.Proc{}, errors.New("released")
}
func (*blockingSys) Args(int) (string, []string, error) { return "", nil, errors.New("no") }
func (*blockingSys) LockHeld(string) (bool, error)      { return false, errors.New("no") }
func (*blockingSys) Openers(string) ([]int, error)      { return nil, errors.New("no") }
