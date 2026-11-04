package a2a

import (
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

func TestPendingLimitCountsUnresolvedTasks(t *testing.T) {
	e := newEnv(t, receiver.Limits{MaxPending: 1}, Limits{}, nil)
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Unknown})
	client, ctx := e.peer()
	first, err := send(t, client, ctx, textMsg("ambiguous", nil), true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, client, ctx, first.ID, sdk.TaskStateWorking)
	for i := 0; i < 2; i++ {
		if _, err := send(t, client, ctx, textMsg("ambiguous", nil), true); err == nil {
			t.Fatal("send beyond MaxPending created a task")
		}
	}
	if n := len(e.srv.PendingMessageIDs()); n != 1 {
		t.Fatalf("unresolved tasks = %d, want 1", n)
	}
	if err := e.srv.Resolve(first.Metadata[MetaKey].(map[string]any)["message_id"].(string), adapter.Accepted, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := send(t, client, ctx, textMsg("after resolve", nil), true); err != nil {
		t.Fatalf("send after the pending task resolved: %v", err)
	}
}

func TestOversizedTextRejectedBeforeTask(t *testing.T) {
	e := newEnv(t, receiver.Limits{MaxMessageBytes: 16}, Limits{}, nil)
	client, ctx := e.peer()
	if _, err := send(t, client, ctx, textMsg(strings.Repeat("x", 64), nil), true); err == nil {
		t.Fatal("oversized text was accepted")
	}
	if n := len(e.srv.PendingMessageIDs()); n != 0 {
		t.Fatalf("%d tasks created for a rejected message", n)
	}
}

func TestOversizedMetadataRejectedBeforeTask(t *testing.T) {
	dir := t.TempDir()
	b := journal.Binding{SessionRef: "session"}
	j, _, err := journal.Open(dir, "agent", b, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := NewJournalTaskStore(j, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	am := textMsg("small text", nil)
	am.Metadata = map[string]any{"extra": strings.Repeat("x", 17<<20)}
	m, err := decode(am)
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(ts, slog.New(slog.DiscardHandler))
	if _, _, err := st.create("local-user", m, am, 32, 256); err == nil {
		t.Fatal("record over the journal line limit was accepted")
	}
	if len(st.pending()) != 0 {
		t.Fatal("task created for a rejected record")
	}
	j.Close()
	reopened, _, err := journal.Open(dir, "agent", b, journal.Options{})
	if err != nil {
		t.Fatalf("journal unreopenable: %v", err)
	}
	reopened.Close()
}

func TestRequestBodyBounded(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	client, ctx := e.peer()
	am := textMsg("small", nil)
	am.Metadata = map[string]any{"extra": strings.Repeat("x", MaxRequestBytes)}
	_, err := send(t, client, ctx, am, true)
	if err == nil {
		t.Fatal("request over MaxRequestBytes was accepted")
	}
	if st := errorState(err); st != adapter.Rejected {
		t.Fatalf("oversized request mapped to %q, want rejected: %v", st, err)
	}
	if n := len(e.srv.PendingMessageIDs()); n != 0 {
		t.Fatalf("%d tasks created for an oversized request", n)
	}
}
