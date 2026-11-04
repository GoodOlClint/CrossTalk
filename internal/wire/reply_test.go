package wire

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/shim"
)

func records(t *testing.T, stateDir, agentID string) []journal.Record {
	t.Helper()
	b, err := os.ReadFile(journal.Path(stateDir, agentID))
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Record
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r journal.Record
		if json.Unmarshal([]byte(l), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func routeStates(t *testing.T, stateDir, agentID, messageID string) []string {
	var out []string
	for _, r := range records(t, stateDir, agentID) {
		if r.Type == journal.TypeRoute && r.MessageID == messageID {
			out = append(out, r.State)
		}
	}
	return out
}

func taskStates(t *testing.T, stateDir, agentID, messageID string) []string {
	var out []string
	for _, r := range records(t, stateDir, agentID) {
		if r.Type == journal.TypeTask && r.MessageID == messageID {
			out = append(out, r.State)
		}
	}
	return out
}

func repliesFrom(n *node, sender string) int {
	c := 0
	for _, w := range n.codex.Writes() {
		if w.Message.From == sender {
			c++
		}
	}
	return c
}

func TestDeadlineClosesAskRouteAndRefusesLateReply(t *testing.T) {
	dir, alice, bob := pair(t)
	ask := alice.ok("ask", map[string]any{"to": "bob", "text": "slow?", "deadline_seconds": 1})
	askID := ask["message_id"].(string)
	waitFor(t, "deadline closes bob's route", func() bool {
		return slices.Equal(routeStates(t, dir, bob.id(), askID), []string{journal.StateOpen, journal.StateClosed})
	})

	isErr, out := bob.call("reply", map[string]any{"message_id": askID, "text": "too late"})
	if !isErr || out["error"] != mcpserver.CodeRejected || out["reason"] != shim.ReasonAskClosed || out["message"] != "ask is closed; send your answer with send_message to alice" {
		t.Fatalf("late reply %v %v", isErr, out)
	}
	for _, r := range records(t, dir, bob.id()) {
		if (r.Type == journal.TypeObligation || r.Type == journal.TypeOutbound) && r.InReplyTo == askID {
			t.Fatalf("late reply journaled %+v", r)
		}
	}
	if n := repliesFrom(alice, "bob"); n != 0 {
		t.Fatalf("alice got %d messages from bob", n)
	}
	if st := taskStates(t, dir, bob.id(), askID); st[len(st)-1] != journal.StateFailed {
		t.Fatalf("task states %v", st)
	}
}

func TestReplyBeforeDeadlineCompletesTask(t *testing.T) {
	dir, alice, bob := pair(t)
	ask := alice.ok("ask", map[string]any{"to": "bob", "text": "quick?", "deadline_seconds": 1})
	askID := ask["message_id"].(string)
	if rep := bob.ok("reply", map[string]any{"message_id": askID, "text": "yes"}); rep["state"] != "accepted" {
		t.Fatalf("reply %v", rep)
	}
	time.Sleep(1500 * time.Millisecond)
	st := taskStates(t, dir, bob.id(), askID)
	if slices.Contains(st, journal.StateFailed) || st[len(st)-1] != journal.StateCompleted {
		t.Fatalf("task states %v", st)
	}
	if rs := routeStates(t, dir, bob.id(), askID); !slices.Equal(rs[len(rs)-1:], []string{journal.StateClosed}) {
		t.Fatalf("route states %v", rs)
	}
	if n := repliesFrom(alice, "bob"); n != 1 {
		t.Fatalf("alice got %d replies", n)
	}
}

// continueAsk sends a continuation of alice's ask askID to bob's task as
// alice, and returns its message id.
func continueAsk(t *testing.T, dir string, alice, bob *node, askID, text string) (string, string) {
	t.Helper()
	taskID := outboundTask(t, dir, alice.id(), askID)
	tok, err := readOwnToken(descriptor(t, dir, alice.id()).CredentialRef)
	if err != nil {
		t.Fatal(err)
	}
	id := msg.NewUUIDv7()
	m := msg.Message{MessageID: id, ContextID: askID, Kind: msg.Ask, Delivery: msg.Immediate, TaskID: &taskID, Parts: []msg.Part{{Type: "text", Text: text}}}
	if _, _, err := (&a2a.Client{Name: "alice"}).Send(context.Background(), descriptor(t, dir, bob.id()).A2AURL, tok, m, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "continuation injected", func() bool {
		for _, w := range bob.codex.Writes() {
			if w.Message.MessageID == id {
				return true
			}
		}
		return false
	})
	return id, taskID
}

func TestReplyToContinuationIDCompletesWholeTask(t *testing.T) {
	dir, alice, bob := pair(t)
	askID := alice.ok("ask", map[string]any{"to": "bob", "text": "first?"})["message_id"].(string)
	contID, taskID := continueAsk(t, dir, alice, bob, askID, "revised?")
	if rep := bob.ok("reply", map[string]any{"message_id": contID, "text": "done"}); rep["state"] != "accepted" {
		t.Fatalf("reply %v", rep)
	}
	tok, _ := readOwnToken(descriptor(t, dir, alice.id()).CredentialRef)
	task, err := (&a2a.Client{Name: "alice"}).GetTask(context.Background(), descriptor(t, dir, bob.id()).A2AURL, tok, taskID)
	if err != nil || task.TaskState != sdk.TaskStateCompleted || task.Reply != "done" {
		t.Fatalf("task %+v %v", task, err)
	}
	for _, id := range []string{askID, contID} {
		if rs := routeStates(t, dir, bob.id(), id); len(rs) == 0 || rs[len(rs)-1] != journal.StateClosed {
			t.Fatalf("route %s states %v", id, rs)
		}
	}
}

func TestReplyToOriginalThenContinuationDeliversOnce(t *testing.T) {
	dir, alice, bob := pair(t)
	askID := alice.ok("ask", map[string]any{"to": "bob", "text": "first?"})["message_id"].(string)
	contID, _ := continueAsk(t, dir, alice, bob, askID, "revised?")
	first := bob.ok("reply", map[string]any{"message_id": askID, "text": "A"})
	if first["state"] != "accepted" {
		t.Fatalf("reply %v", first)
	}
	isErr, out := bob.call("reply", map[string]any{"message_id": contID, "text": "B"})
	if !isErr || out["reason"] != shim.ReasonAskClosed {
		t.Fatalf("second reply to the same task %v %v", isErr, out)
	}
	if again := bob.ok("reply", map[string]any{"message_id": contID, "text": "A"}); again["message_id"] != first["message_id"] {
		t.Fatalf("identical retry through the continuation id %v, first %v", again, first)
	}
	if n := repliesFrom(alice, "bob"); n != 1 {
		t.Fatalf("alice got %d replies", n)
	}
}

func TestContinuationReplySettlesOriginalAsk(t *testing.T) {
	dir, alice, bob := pair(t)
	askID := alice.ok("ask", map[string]any{"to": "bob", "text": "first?", "deadline_seconds": 1})["message_id"].(string)
	contID, taskID := continueAsk(t, dir, alice, bob, askID, "revised?")
	if rep := bob.ok("reply", map[string]any{"message_id": contID, "text": "done"}); rep["state"] != "accepted" {
		t.Fatalf("reply %v", rep)
	}
	receipt := false
	for _, r := range records(t, dir, alice.id()) {
		if r.Type == journal.TypeReceipt && r.InReplyTo == askID && r.Sender == bob.id() {
			receipt = true
		}
	}
	if !receipt {
		t.Fatalf("no receipt on the original ask %s", askID)
	}
	var got *msg.Message
	for _, w := range alice.codex.Writes() {
		if w.Message.From == "bob" {
			m := w.Message
			got = &m
		}
	}
	if got == nil || got.InReplyTo == nil || *got.InReplyTo != askID || got.ContextID != askID {
		t.Fatalf("alice got reply %+v, want in_reply_to and context %s", got, askID)
	}
	time.Sleep(1300 * time.Millisecond)
	if n := repliesFrom(alice, "crosstalk"); n != 0 {
		t.Fatalf("alice got %d expiry notices after the answer", n)
	}
	tok, _ := readOwnToken(descriptor(t, dir, alice.id()).CredentialRef)
	task, err := (&a2a.Client{Name: "alice"}).GetTask(context.Background(), descriptor(t, dir, bob.id()).A2AURL, tok, taskID)
	if err != nil || task.TaskState != sdk.TaskStateCompleted || task.Reply != "done" {
		t.Fatalf("task %+v %v", task, err)
	}
}
