package receiver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func strand(t *testing.T, e *env, sender string, m msg.Message) {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.j.AcceptRecord(journal.Record{Sender: sender, MessageID: m.MessageID, State: journal.StateSubmitted, Message: body}); err != nil {
		t.Fatal(err)
	}
}

func submittedRecord(t *testing.T, e *env) msg.Message {
	t.Helper()
	m := newMsg(msg.Inform, "stranded")
	strand(t, e, alice.AgentID, m)
	return m
}

func recoverAgain(t *testing.T, e *env) {
	t.Helper()
	e.j.Close()
	if err := e.r.Recover(e.open(selfBinding, "inst-1")); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverSubmittedWithoutClaimOrTaskSurfacesRecovered(t *testing.T) {
	e := newEnv(t)
	m := submittedRecord(t, e)
	recoverAgain(t, e)
	if st, _ := e.j.Dedupe(alice.AgentID, m.MessageID); st != "recovered" {
		t.Fatalf("state %q, want recovered", st)
	}
	mb, err := e.r.Mailbox("", 0)
	if err != nil || len(mb) != 1 || mb[0].MessageID != m.MessageID || mb[0].State != "recovered" || mb[0].Message == nil || mb[0].Message.Text() != "stranded" {
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

func TestRecoveredIsNeverAdmitted(t *testing.T) {
	if Admitted("recovered") {
		t.Fatal("recovered counts as admitted")
	}
	if !Admitted("unknown") {
		t.Fatal("a claimed unknown no longer counts as admitted")
	}
}

func TestRecoveredRetryIsDuplicateWithoutInjection(t *testing.T) {
	e := newEnv(t)
	m := submittedRecord(t, e)
	recoverAgain(t, e)
	if r := e.recv(UDS, alice, m); !r.Duplicate || r.State != "recovered" || e.writes() != 0 {
		t.Fatalf("retry %+v, injections %d", r, e.writes())
	}
}

func TestRecoveredOutlivesRetention(t *testing.T) {
	e := newEnv(t)
	m := submittedRecord(t, e)
	recoverAgain(t, e)
	e.j.Close()
	later := time.Now().Add(2 * journal.DefaultRetention)
	j, _, err := journal.Open(e.dir, selfID, jbind(selfBinding, "inst-2"), journal.Options{Now: func() time.Time { return later }})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	mb, err := New(Config{Journal: j, Adapter: e.fake, Self: e.self}).Mailbox("", 0)
	if err != nil || len(mb) != 1 || mb[0].MessageID != m.MessageID || mb[0].Message == nil {
		t.Fatalf("mailbox %+v err %v", mb, err)
	}
}

func TestRecoverRejectsWhatTheChainWouldReject(t *testing.T) {
	hop := newMsg(msg.Inform, "looped")
	hop.HopChain = []string{msg.Hop(selfID)}
	cases := []struct {
		name   string
		sender string
		m      msg.Message
	}{
		{"unverified command", unverifiedKey, newMsg(msg.Command, "forged")},
		{"verified command", alice.AgentID, newMsg(msg.Command, "uds cannot carry this")},
		{"ask", alice.AgentID, newMsg(msg.Ask, "uds cannot carry this")},
		{"unknown kind", alice.AgentID, newMsg(msg.Kind("control"), "no such kind")},
		{"hop repeat", alice.AgentID, hop},
		{"oversized", alice.AgentID, newMsg(msg.Inform, strings.Repeat("x", DefaultLimits.MaxMessageBytes+1))},
	}
	other := selfBinding
	other.SessionRef = "sess-other"
	for _, c := range cases {
		for _, b := range []struct {
			name    string
			binding adapter.Binding
		}{{"same conversation", selfBinding}, {"other conversation", other}} {
			t.Run(c.name+"/"+b.name, func(t *testing.T) {
				e := newEnv(t)
				strand(t, e, c.sender, c.m)
				e.j.Close()
				if err := e.r.Recover(e.open(b.binding, "inst-2")); err != nil {
					t.Fatal(err)
				}
				if st, _ := e.j.Dedupe(c.sender, c.m.MessageID); st != "rejected" {
					t.Fatalf("state %q, want rejected", st)
				}
				if mb, err := e.r.Mailbox("", 0); err != nil || len(mb) != 0 {
					t.Fatalf("mailbox %+v err %v", mb, err)
				}
				if e.writes() != 0 {
					t.Fatalf("injections %d", e.writes())
				}
				before := len(mustRecords(t, e))
				e.j.Close()
				if err := e.r.Recover(e.open(b.binding, "inst-3")); err != nil {
					t.Fatal(err)
				}
				if after := len(mustRecords(t, e)); after != before {
					t.Fatalf("second recovery wrote: records %d -> %d", before, after)
				}
			})
		}
	}
}

func TestRecoverSubmittedWithoutBodyStaysOut(t *testing.T) {
	e := newEnv(t)
	id := msg.NewUUIDv7()
	if _, _, err := e.j.AcceptRecord(journal.Record{Sender: alice.AgentID, MessageID: id, State: journal.StateSubmitted}); err != nil {
		t.Fatal(err)
	}
	recoverAgain(t, e)
	if st, _ := e.j.Dedupe(alice.AgentID, id); st != "rejected" {
		t.Fatalf("state %q, want rejected", st)
	}
	if mb, err := e.r.Mailbox("", 0); err != nil || len(mb) != 0 {
		t.Fatalf("mailbox %+v err %v", mb, err)
	}
}

func TestRecoverPassingMessageOfOtherConversationIsUnreachable(t *testing.T) {
	e := newEnv(t)
	m := submittedRecord(t, e)
	other := selfBinding
	other.SessionRef = "sess-other"
	e.j.Close()
	if err := e.r.Recover(e.open(other, "inst-2")); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.j.Dedupe(alice.AgentID, m.MessageID); st != "unreachable" {
		t.Fatalf("state %q, want unreachable", st)
	}
}

func TestRecoverContinuationStaysSubmittedAndOutOfMailbox(t *testing.T) {
	e := newEnv(t)
	task := "task-1"
	m := newMsg(msg.Ask, "revised?")
	m.TaskID = &task
	strand(t, e, alice.AgentID, m)
	if err := e.j.Append(journal.Record{Type: journal.TypeTask, TaskID: task, MessageID: "first-message", ContextID: m.ContextID, State: journal.StateSubmitted}); err != nil {
		t.Fatal(err)
	}
	recoverAgain(t, e)
	st, _ := e.j.Dedupe(alice.AgentID, m.MessageID)
	if st != "submitted" || Admitted(st) {
		t.Fatalf("state %q, want submitted and not admitted", st)
	}
	if mb, err := e.r.Mailbox("", 0); err != nil || len(mb) != 0 {
		t.Fatalf("mailbox %+v err %v", mb, err)
	}
	before := len(mustRecords(t, e))
	recoverAgain(t, e)
	if after := len(mustRecords(t, e)); after != before || e.writes() != 0 {
		t.Fatalf("second recovery wrote: records %d -> %d, injections %d", before, after, e.writes())
	}
}

func TestRecoverContinuationOfOtherConversationIsUnreachable(t *testing.T) {
	e := newEnv(t)
	task := "task-1"
	m := newMsg(msg.Ask, "revised?")
	m.TaskID = &task
	strand(t, e, alice.AgentID, m)
	other := selfBinding
	other.SessionRef = "sess-other"
	e.j.Close()
	if err := e.r.Recover(e.open(other, "inst-2")); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.j.Dedupe(alice.AgentID, m.MessageID); st != "unreachable" {
		t.Fatalf("state %q, want unreachable", st)
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
