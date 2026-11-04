package receiver

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClockEnv(t *testing.T) (*env, *clock) {
	e := newEnv(t)
	c := &clock{t: time.Now().UTC()}
	e.r = New(Config{Journal: e.j, Adapter: e.fake, Self: e.self, Now: c.now})
	return e, c
}

func (e *env) lastBounce() string {
	w := e.fake.Writes()
	return w[len(w)-1].Message.Text()
}

func routeClosed(t *testing.T, e *env, messageID string) bool {
	rs, err := e.j.Records()
	if err != nil {
		t.Fatal(err)
	}
	last := ""
	for _, r := range rs {
		if r.Type == journal.TypeRoute && r.MessageID == messageID {
			last = r.State
		}
	}
	return last == journal.StateClosed
}

func TestInformRouteExpiresAfterTTL(t *testing.T) {
	e, c := newClockEnv(t)
	old, ask := newMsg(msg.Inform, "fyi"), newMsg(msg.Ask, "q")
	e.recv(A2A, alice, old)
	e.recv(A2A, alice, ask)
	c.advance(InformRouteTTL - time.Minute)
	if !e.r.RouteOpen(old.MessageID) {
		t.Fatal("inform route closed before its TTL")
	}
	c.advance(2 * time.Minute)
	e.native(ownSession, "hello?")
	bt := e.lastBounce()
	if strings.Contains(bt, old.MessageID) || !strings.Contains(bt, ask.MessageID) {
		t.Fatalf("61-minute-old inform listed or ask dropped: %s", bt)
	}
	if !routeClosed(t, e, old.MessageID) {
		t.Fatal("expired inform route not journaled closed")
	}
	if e.r.RouteOpen(old.MessageID) || e.r.CloseRoute(old.MessageID) {
		t.Fatal("expired inform route still open")
	}
	if !e.r.RouteOpen(ask.MessageID) {
		t.Fatal("ask route expired on the inform clock")
	}
}

func TestExpiredInformRouteSweptOnRecover(t *testing.T) {
	e, c := newClockEnv(t)
	m := newMsg(msg.Inform, "fyi")
	e.recv(A2A, alice, m)
	e.j.Close()
	rp := e.open(selfBinding, "inst-2")
	c.advance(61 * time.Minute)
	e.r = New(Config{Journal: e.j, Adapter: e.fake, Self: e.self, Now: c.now})
	if err := e.r.Recover(rp); err != nil {
		t.Fatal(err)
	}
	if !routeClosed(t, e, m.MessageID) {
		t.Fatal("recover kept an expired inform route open")
	}
}

func TestBounceListsAllAsksAndCapsInforms(t *testing.T) {
	e, c := newClockEnv(t)
	var asks, informs []msg.Message
	for i := range 12 {
		a, in := newMsg(msg.Ask, fmt.Sprint("q", i)), newMsg(msg.Inform, fmt.Sprint("fyi", i))
		e.recv(A2A, alice, a)
		e.recv(A2A, carol, in)
		asks, informs = append(asks, a), append(informs, in)
		c.advance(time.Second)
	}
	e.native(ownSession, "which?")
	bt := e.lastBounce()
	for _, a := range asks {
		if !strings.Contains(bt, "message_id="+a.MessageID) {
			t.Fatalf("ask %s omitted: %s", a.MessageID, bt)
		}
	}
	for i, in := range informs {
		if listed := strings.Contains(bt, "message_id="+in.MessageID); listed != (i >= 2) {
			t.Fatalf("inform %d listed=%v: %s", i, listed, bt)
		}
	}
	if !strings.HasSuffix(bt, "\nand 2 more informs") || strings.Contains(bt, "receive_message") {
		t.Fatalf("bounce tail: %s", bt)
	}
	if strings.Index(bt, asks[11].MessageID) > strings.Index(bt, informs[11].MessageID) {
		t.Fatalf("asks are not listed first: %s", bt)
	}
}

func TestInformAdmissionSweepsExpiredRoutes(t *testing.T) {
	e, c := newClockEnv(t)
	var sent []msg.Message
	for i := range 4 {
		if i > 0 {
			c.advance(2 * time.Hour)
		}
		m := newMsg(msg.Inform, fmt.Sprint("fyi", i))
		e.recv(A2A, alice, m)
		sent = append(sent, m)
	}
	e.r.mu.Lock()
	n := len(e.r.routes)
	e.r.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d routes in memory after six hours of informs, want 1", n)
	}
	for i, m := range sent {
		if closed := routeClosed(t, e, m.MessageID); closed != (i < 3) {
			t.Errorf("inform %d journaled closed = %t", i, closed)
		}
	}
}
