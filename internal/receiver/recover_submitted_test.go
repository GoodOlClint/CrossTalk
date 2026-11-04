package receiver

import (
	"encoding/json"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func submittedRecord(t *testing.T, e *env) msg.Message {
	t.Helper()
	m := newMsg(msg.Inform, "stranded")
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.j.AcceptRecord(journal.Record{Sender: alice.AgentID, MessageID: m.MessageID, State: journal.StateSubmitted, Message: body}); err != nil {
		t.Fatal(err)
	}
	return m
}

func recoverAgain(t *testing.T, e *env) {
	t.Helper()
	e.j.Close()
	if err := e.r.Recover(e.open(selfBinding, "inst-1")); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverSubmittedWithoutClaimOrTaskSurfacesUnknown(t *testing.T) {
	e := newEnv(t)
	m := submittedRecord(t, e)
	recoverAgain(t, e)
	if st, _ := e.j.Dedupe(alice.AgentID, m.MessageID); st != "unknown" {
		t.Fatalf("state %q, want unknown", st)
	}
	mb, err := e.r.Mailbox("", 0)
	if err != nil || len(mb) != 1 || mb[0].MessageID != m.MessageID || mb[0].State != "unknown" {
		t.Fatalf("mailbox %+v err %v", mb, err)
	}
	before := len(mustRecords(t, e))
	recoverAgain(t, e)
	if after := len(mustRecords(t, e)); after != before || e.writes() != 0 {
		t.Fatalf("second recovery wrote: records %d -> %d, injections %d", before, after, e.writes())
	}
}

func TestRecoverSubmittedWithTaskRecordUnchanged(t *testing.T) {
	e := newEnv(t)
	m := submittedRecord(t, e)
	if err := e.j.Append(journal.Record{Type: journal.TypeTask, TaskID: "task-1", MessageID: m.MessageID, ContextID: m.ContextID, State: journal.StateSubmitted}); err != nil {
		t.Fatal(err)
	}
	recoverAgain(t, e)
	if st, _ := e.j.Dedupe(alice.AgentID, m.MessageID); st != "submitted" {
		t.Fatalf("state %q, want submitted", st)
	}
}

func TestRecoverClaimedSubmittedUnchanged(t *testing.T) {
	e := newEnv(t)
	m := submittedRecord(t, e)
	if ok, err := e.j.Claim(m.MessageID); !ok || err != nil {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err := e.j.Resolve(m.MessageID, "accepted"); err != nil {
		t.Fatal(err)
	}
	recoverAgain(t, e)
	if st, _ := e.j.Dedupe(alice.AgentID, m.MessageID); st != "submitted" {
		t.Fatalf("state %q, want submitted", st)
	}
}

func mustRecords(t *testing.T, e *env) []journal.Record {
	t.Helper()
	rs, err := e.j.Records()
	if err != nil {
		t.Fatal(err)
	}
	return rs
}
