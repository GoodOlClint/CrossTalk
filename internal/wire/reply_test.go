package wire

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
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
