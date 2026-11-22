package a2a

import (
	"context"
	"net/http"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/quiesce"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

func cardOK(t *testing.T, url string) bool {
	t.Helper()
	resp, err := http.Get(url + "/.well-known/agent-card.json")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// While paused a send is refused at dial (unreachable); an unpause right
// after the pause, before Serve sees the close, still serves at the same URL.
func TestServerPauseRefusesDialAndUnpauseServesSameURL(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	if err := e.srv.Pause(); err != nil {
		t.Fatal(err)
	}
	if _, st, err := (&Client{}).Send(context.Background(), e.url, e.peerTok, plain("hi", msg.Inform), true); err == nil || st != adapter.Unreachable {
		t.Fatalf("send while paused: %s %v, want unreachable", st, err)
	}
	for range 20 {
		if err := e.srv.Unpause(); err != nil {
			t.Fatal(err)
		}
		if err := e.srv.Pause(); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.srv.Unpause(); err != nil {
		t.Fatal(err)
	}
	if !cardOK(t, e.url) {
		t.Fatal("not serving at the published URL after unpause")
	}
}

// A close during a pause, then an unpause, does not panic or listen again.
func TestServerCloseWhilePausedThenUnpause(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	e.srv.Pause()
	e.srv.Close()
	if err := e.srv.Unpause(); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Unpause(); err != nil {
		t.Fatal(err)
	}
	if cardOK(t, e.url) {
		t.Fatal("listening again after Close")
	}
}

// A deadline already past when armed runs inside the work that armed it; it
// must not wait out a refresh pause that is waiting for that work.
func TestExpiredDeadlineDoesNotWaitOutAPause(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	if !quiesce.Pause() {
		t.Fatal("pause in progress")
	}
	defer quiesce.Unpause()
	done := make(chan struct{})
	go func() {
		e.srv.armDeadline(&task{deadline: time.Now().Add(-time.Second), state: sdk.TaskStateCompleted})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an expired deadline waited for the pause to end")
	}
}
