package wire

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func TestReconcileAppliesJournaledOutcome(t *testing.T) {
	j, _, err := journal.Open(t.TempDir(), "aaaaaaaaaaaaaaaa", journal.Binding{SessionRef: "s"}, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	done, open, unclaimed := msg.NewUUIDv7(), msg.NewUUIDv7(), msg.NewUUIDv7()
	for i, id := range []string{done, open, unclaimed} {
		state := journal.StateWorking
		if id == unclaimed {
			state = journal.StateSubmitted
		}
		if err := j.Append(journal.Record{Type: journal.TypeTask, TaskID: "task-" + string(rune('a'+i)), MessageID: id, ContextID: id, Principal: "local-user", Kind: string(msg.Inform), State: state}); err != nil {
			t.Fatal(err)
		}
	}
	for id, outcome := range map[string]string{done: "accepted", open: journal.StateUnknown} {
		if _, err := j.Claim(id); err != nil {
			t.Fatal(err)
		}
		if err := j.Resolve(id, outcome); err != nil {
			t.Fatal(err)
		}
	}
	ts, err := a2a.NewJournalTaskStore(j, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := a2a.NewServer(a2a.Config{Tasks: ts})
	defer srv.Close()
	reconcile(srv, j, slog.New(slog.DiscardHandler))
	got := srv.PendingMessageIDs()
	if len(got) != 2 || !slices.Contains(got, open) || !slices.Contains(got, unclaimed) {
		t.Fatalf("pending %v, want %s and %s", got, open, unclaimed)
	}
	l, ok := j.Latest(journal.Record{Type: journal.TypeTask, TaskID: "task-a"})
	if !ok || l.State != journal.StateCompleted || l.Delivery != "accepted" {
		t.Fatalf("journaled task %+v", l)
	}
}
