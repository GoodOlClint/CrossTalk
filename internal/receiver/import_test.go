package receiver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func delivered(sender Sender, text string, at time.Time) Delivered {
	m := newMsg(msg.Inform, text)
	m.From = sender.Name
	return Delivered{Sender: sender.AgentID, Message: m, DeliveredAt: at, State: string(adapter.Accepted)}
}

func routeRecords(t *testing.T, e *env, id string) []journal.Record {
	t.Helper()
	rs, err := e.j.Records()
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Record
	for _, r := range rs {
		if r.Type == journal.TypeRoute && r.MessageID == id {
			out = append(out, r)
		}
	}
	return out
}

func openedAt(e *env, id string) (time.Time, bool) {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	for _, rt := range e.r.routes {
		if rt.MessageID == id {
			return rt.OpenedAt, true
		}
	}
	return time.Time{}, false
}

func TestImportDeliveredOpensRouteWithSuppliedTime(t *testing.T) {
	e, c := newClockEnv(t)
	at := c.now().Add(-20 * time.Minute)
	d := delivered(alice, "hi", at)
	if err := e.r.ImportDelivered(d); err != nil {
		t.Fatal(err)
	}
	if got, ok := openedAt(e, d.Message.MessageID); !ok || !got.Equal(at) {
		t.Fatalf("OpenedAt %v, want %v", got, at)
	}
	if !e.r.RouteOpen(d.Message.MessageID) {
		t.Fatal("route not open")
	}
	e.j.Close()
	rp := e.open(selfBinding, "inst-2")
	e.r = New(Config{Journal: e.j, Adapter: e.fake, Self: e.self, Now: c.now})
	if err := e.r.Recover(rp); err != nil {
		t.Fatal(err)
	}
	if got, ok := openedAt(e, d.Message.MessageID); !ok || !got.Equal(at) {
		t.Fatalf("after Recover OpenedAt %v, want %v", got, at)
	}

	future := delivered(alice, "later", c.now().Add(time.Hour))
	if err := e.r.ImportDelivered(future); err != nil {
		t.Fatal(err)
	}
	if got, _ := openedAt(e, future.Message.MessageID); got.After(c.now()) {
		t.Fatalf("a delivery time ahead of the clock opened the route at %v", got)
	}
}

func TestImportDeliveredIsIdempotent(t *testing.T) {
	e, c := newClockEnv(t)
	d := delivered(alice, "hi", c.now().Add(-time.Minute))
	id := d.Message.MessageID
	for range 2 {
		if err := e.r.ImportDelivered(d); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ := e.j.Records()
	if len(rs) != 2 {
		t.Fatalf("two imports wrote %d records, want 2", len(rs))
	}

	if !e.r.CloseRoute(id) {
		t.Fatal("route not open")
	}
	if err := e.r.ImportDelivered(d); err != nil {
		t.Fatal(err)
	}
	if e.r.RouteOpen(id) {
		t.Fatal("an answered route was reopened")
	}

	exp := delivered(alice, "old", c.now().Add(-59*time.Minute))
	e.r.ImportDelivered(exp)
	c.advance(2 * time.Minute)
	if e.r.RouteOpen(exp.Message.MessageID) {
		t.Fatal("route outlived its TTL")
	}
	e.r.ImportDelivered(exp)
	if e.r.RouteOpen(exp.Message.MessageID) {
		t.Fatal("an expired route was reopened")
	}

	part := delivered(carol, "partial", c.now().Add(-time.Minute))
	body, _ := json.Marshal(part.Message)
	if err := e.j.Append(journal.Record{Type: journal.TypeDedupe, Sender: carol.AgentID, MessageID: part.Message.MessageID, State: part.State, Message: body, Imported: true}); err != nil {
		t.Fatal(err)
	}
	if !e.r.ImportIncomplete(part.Message.MessageID) {
		t.Fatal("a dedupe record with no route is not reported incomplete")
	}
	before, _ := e.j.Records()
	if err := e.r.ImportDelivered(part); err != nil {
		t.Fatal(err)
	}
	after, _ := e.j.Records()
	if len(after)-len(before) != 1 || after[len(after)-1].Type != journal.TypeRoute {
		t.Fatalf("a retry wrote %d records, want only the route", len(after)-len(before))
	}
	if e.r.ImportIncomplete(part.Message.MessageID) || !e.r.RouteOpen(part.Message.MessageID) {
		t.Fatal("retry did not complete the import")
	}
}

func TestImportDeliveredNeverInjects(t *testing.T) {
	e, c := newClockEnv(t)
	for _, s := range []Sender{alice, carol} {
		if err := e.r.ImportDelivered(delivered(s, "hi", c.now())); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.writes(); n != 0 {
		t.Fatalf("%d injects", n)
	}
}

func TestImportedRouteExpiresOneHourAfterDelivery(t *testing.T) {
	e, c := newClockEnv(t)
	d := delivered(alice, "hi", c.now().Add(-55*time.Minute))
	id := d.Message.MessageID
	if err := e.r.ImportDelivered(d); err != nil {
		t.Fatal(err)
	}
	c.advance(4 * time.Minute)
	if !e.r.RouteOpen(id) {
		t.Fatal("route closed early")
	}
	e.j.Close()
	rp := e.open(selfBinding, "inst-2")
	e.r = New(Config{Journal: e.j, Adapter: e.fake, Self: e.self, Now: c.now})
	if err := e.r.Recover(rp); err != nil {
		t.Fatal(err)
	}
	if !e.r.RouteOpen(id) {
		t.Fatal("route lost on restart")
	}
	c.advance(2 * time.Minute)
	if e.r.RouteOpen(id) {
		t.Fatal("route open 61 minutes after delivery, after a restart")
	}
}

func TestImportDeliveredRefusesUnroutableSenders(t *testing.T) {
	e, c := newClockEnv(t)
	for _, sender := range []string{"", PrincipalLocalUser, RemoteKey("abcd", alice.AgentID)} {
		d := delivered(alice, "hi", c.now())
		d.Sender = sender
		if err := e.r.ImportDelivered(d); err == nil {
			t.Fatalf("sender %q imported", sender)
		}
	}
	ask := delivered(alice, "q", c.now())
	ask.Message.Kind = msg.Ask
	if err := e.r.ImportDelivered(ask); err == nil {
		t.Fatal("an ask imported")
	}
	if rs, _ := e.j.Records(); len(rs) != 0 {
		t.Fatalf("refused imports wrote %d records", len(rs))
	}
}

func TestImportDeliveredDropsIDHeldByAnotherSender(t *testing.T) {
	e, c := newClockEnv(t)
	first := delivered(carol, "genuine", c.now().Add(-time.Minute))
	id := first.Message.MessageID
	if err := e.r.ImportDelivered(first); err != nil {
		t.Fatal(err)
	}
	second := delivered(alice, "forged", c.now())
	second.Message.MessageID = id
	if err := e.r.ImportDelivered(second); err != nil {
		t.Fatal(err)
	}
	if e.r.RouteOpen(id) {
		t.Fatal("the conflicting import left the first import's route open")
	}
	if rs := routeRecords(t, e, id); len(rs) != 2 || rs[0].Sender != carol.AgentID || rs[1].State != journal.StateClosed {
		t.Fatalf("route records %+v", rs)
	}
	if _, held := e.j.Dedupe(alice.AgentID, id); held {
		t.Fatal("the conflicting sender was recorded")
	}

	live := newMsg(msg.Inform, "live")
	if res := e.recv(A2A, alice, live); res.State != adapter.Accepted {
		t.Fatalf("live receive %+v", res)
	}
	clash := delivered(carol, "clash", c.now())
	clash.Message.MessageID = live.MessageID
	if err := e.r.ImportDelivered(clash); err != nil {
		t.Fatal(err)
	}
	if !e.r.RouteOpen(live.MessageID) {
		t.Fatal("an import closed a route the receiver opened itself")
	}
}
