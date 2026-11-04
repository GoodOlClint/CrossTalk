package a2a

import (
	"context"
	"slices"
	"testing"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

func TestReplyToContinuationCompletesTask(t *testing.T) {
	r := &countingReceiver{}
	ts := NewMemoryTaskStore()
	s := NewServer(Config{Receiver: r, Tasks: ts})
	ctx := context.WithValue(context.Background(), senderKey{}, receiver.Sender{Verified: true, Principal: receiver.PrincipalLocalUser})
	h := &handler{s: s}
	first := textMsg("first", map[string]any{"kind": "ask"})
	got, err := h.SendMessage(ctx, &sdk.SendMessageRequest{Message: first, Config: &sdk.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatal(err)
	}
	task := got.(*sdk.Task)
	second := textMsg("revised question", map[string]any{"kind": "ask"})
	second.TaskID, second.ContextID = task.ID, task.ContextID
	if _, err := h.SendMessage(ctx, &sdk.SendMessageRequest{Message: second, Config: &sdk.SendMessageConfig{ReturnImmediately: true}}); err != nil {
		t.Fatal(err)
	}
	if id, open, known := s.AskOpen(second.ID); !known || !open || id != string(task.ID) {
		t.Fatalf("continuation handle id=%q open=%v known=%v, want %s", id, open, known, task.ID)
	}
	s.Close()

	var closed []string
	s2 := NewServer(Config{Receiver: r, Tasks: ts, OnAskClosed: func(ids []string) { closed = append(closed, ids...) }})
	defer s2.Close()
	if id, _, known := s2.AskOpen(second.ID); !known || id != string(task.ID) {
		t.Fatalf("continuation handle lost on replay: id=%q known=%v", id, known)
	}
	if err := s2.CompleteAsk(second.ID, "answer"); err != nil {
		t.Fatalf("reply to delivered continuation cannot complete its task: %v", err)
	}
	slices.Sort(closed)
	want := []string{first.ID, second.ID}
	slices.Sort(want)
	if !slices.Equal(closed, want) {
		t.Fatalf("closed routes %v, want %v", closed, want)
	}
	if _, open, _ := s2.AskOpen(first.ID); open {
		t.Fatal("first message still open after its continuation was answered")
	}
	if err := s2.CompleteAsk(first.ID, "other"); err == nil {
		t.Fatal("second reply to the same task completed it again")
	}
	if err := s2.CompleteAsk(first.ID, "answer"); err != nil {
		t.Fatalf("identical completion is not idempotent: %v", err)
	}
}
