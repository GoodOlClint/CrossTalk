package a2a

import (
	"errors"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func journalTasks(t *testing.T, opt journal.Options) func(*journal.Journal) TaskStore {
	return func(j *journal.Journal) TaskStore {
		ts, err := NewJournalTaskStore(j, opt)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
}

type sent struct {
	id    sdk.TaskID
	msgID string
	state sdk.TaskState
	deliv string
}

func TestTasksSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	e := newEnvOn(t, dir, journalTasks(t, journal.Options{}), receiver.Limits{}, Limits{}, nil)
	e.addAgent("worker", "bbbbbbbbbbbbbbbb", registry.RolePrimary)
	peer, pctx := e.peer()
	agent, actx := e.sdkClient(e.toks["worker"], "worker")

	var all []sent
	sendAs := func(o adapter.Outcome, m *sdk.Message, want sdk.TaskState) sent {
		t.Helper()
		e.fake.SetDefaultInject(o)
		task, err := send(t, peer, pctx, m, true)
		if err != nil {
			t.Fatal(err)
		}
		got := waitState(t, peer, pctx, task.ID, want)
		s := sent{id: task.ID, msgID: m.ID, state: want, deliv: meta(got)["state"].(string)}
		all = append(all, s)
		return s
	}
	sendAs(adapter.Outcome{State: adapter.Accepted}, textMsg("done", nil), sdk.TaskStateCompleted)
	sendAs(adapter.Outcome{State: adapter.Unreachable}, textMsg("gone", nil), sdk.TaskStateFailed)
	unknown := sendAs(adapter.Outcome{State: adapter.Unknown}, textMsg("maybe", nil), sdk.TaskStateWorking)
	ask := sendAs(adapter.Outcome{State: adapter.Accepted}, textMsg("which?", map[string]any{"kind": "ask"}), sdk.TaskStateWorking)
	if err := e.srv.CompleteAsk(ask.msgID, "slice/s12"); err != nil {
		t.Fatal(err)
	}
	waitState(t, peer, pctx, ask.id, sdk.TaskStateCompleted)
	all[len(all)-1].state, all[len(all)-1].deliv = sdk.TaskStateCompleted, string(adapter.Accepted)
	sendAs(adapter.Outcome{State: adapter.Accepted}, textMsg("soon?", map[string]any{"kind": "ask", "deadline_seconds": 2}), sdk.TaskStateWorking)
	deadlined := all[len(all)-1]

	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Accepted})
	own, err := send(t, agent, actx, textMsg("from worker", nil), false)
	if err != nil || own.Status.State != sdk.TaskStateCompleted {
		t.Fatalf("agent send %v %v", own, err)
	}
	e.stop()

	e2 := newEnvOn(t, dir, journalTasks(t, journal.Options{}), receiver.Limits{}, Limits{}, nil)
	e2.addAgent("worker", "bbbbbbbbbbbbbbbb", registry.RolePrimary)
	peer2, pctx2 := e2.peer()
	agent2, actx2 := e2.sdkClient(e2.toks["worker"], "worker")
	for _, s := range all {
		if s == deadlined {
			continue
		}
		got, err := peer2.GetTask(pctx2, &sdk.GetTaskRequest{ID: s.id})
		if err != nil {
			t.Fatalf("GetTask %s after restart: %v", s.msgID, err)
		}
		if got.Status.State != s.state || meta(got)["state"] != s.deliv || meta(got)["message_id"] != s.msgID {
			t.Fatalf("task %s: %s %v, want %s %s", s.msgID, got.Status.State, meta(got), s.state, s.deliv)
		}
		if len(got.History) == 0 || got.History[0].ID != s.msgID {
			t.Fatalf("task %s history %+v", s.msgID, got.History)
		}
	}
	got, _ := peer2.GetTask(pctx2, &sdk.GetTaskRequest{ID: ask.id})
	if got.Status.Message == nil || got.Status.Message.Parts[0].Text() != "slice/s12" {
		t.Fatalf("ask reply after restart %+v", got.Status.Message)
	}
	if _, err := peer2.GetTask(pctx2, &sdk.GetTaskRequest{ID: own.ID}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("peer read the worker's task: %v", err)
	}
	if got, err := agent2.GetTask(actx2, &sdk.GetTaskRequest{ID: own.ID}); err != nil || got.Status.State != sdk.TaskStateCompleted {
		t.Fatalf("worker's own task %v %v", got, err)
	}
	if _, err := agent2.GetTask(actx2, &sdk.GetTaskRequest{ID: unknown.id}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("worker read the peer's task: %v", err)
	}

	retry := textMsg("maybe", nil)
	retry.ID = unknown.msgID
	again, err := send(t, peer2, pctx2, retry, true)
	if err != nil || again.ID != unknown.id {
		t.Fatalf("retry after restart %v %v, want task %s", again, err, unknown.id)
	}

	expired := waitState(t, peer2, pctx2, deadlined.id, sdk.TaskStateFailed)
	if meta(expired)["reason"] != ReasonDeadline {
		t.Fatalf("re-armed deadline %v", meta(expired))
	}
}

func TestTerminalTasksPruned(t *testing.T) {
	dir := t.TempDir()
	e := newEnvOn(t, dir, journalTasks(t, journal.Options{}), receiver.Limits{}, Limits{}, nil)
	cl, ctx := e.peer()
	done, err := send(t, cl, ctx, textMsg("done", nil), false)
	if err != nil || done.Status.State != sdk.TaskStateCompleted {
		t.Fatalf("send %v %v", done, err)
	}
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Unknown})
	open, err := send(t, cl, ctx, textMsg("maybe", nil), true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, cl, ctx, open.ID, sdk.TaskStateWorking)
	e.stop()

	later := journal.Options{Now: func() time.Time { return time.Now().Add(journal.DefaultRetention + time.Hour) }}
	e2 := newEnvOn(t, dir, journalTasks(t, later), receiver.Limits{}, Limits{}, nil)
	cl2, ctx2 := e2.peer()
	if _, err := cl2.GetTask(ctx2, &sdk.GetTaskRequest{ID: done.ID}); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("old completed task kept: %v", err)
	}
	if got, err := cl2.GetTask(ctx2, &sdk.GetTaskRequest{ID: open.ID}); err != nil || got.Status.State != sdk.TaskStateWorking {
		t.Fatalf("old working task %v %v", got, err)
	}
}

func TestResolve(t *testing.T) {
	ts := NewMemoryTaskStore()
	dir := t.TempDir()
	e := newEnvOn(t, dir, func(*journal.Journal) TaskStore { return ts }, receiver.Limits{}, Limits{}, nil)
	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Unknown})
	cl, ctx := e.peer()

	m := textMsg("maybe", nil)
	woke := make(chan *sdk.Task, 1)
	go func() {
		task, err := send(t, cl, ctx, m, false)
		if err != nil {
			t.Error(err)
		}
		woke <- task
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if l, err := cl.ListTasks(ctx, &sdk.ListTasksRequest{}); err == nil && len(l.Tasks) == 1 && meta(l.Tasks[0])["state"] == "unknown" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task never reached unknown")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.srv.Resolve(m.ID, adapter.Unknown, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case task := <-woke:
		t.Fatalf("unknown woke the waiter: %s", task.Status.State)
	case <-time.After(100 * time.Millisecond):
	}
	if err := e.srv.Resolve(m.ID, adapter.Accepted, ""); err != nil {
		t.Fatal(err)
	}
	var mt sdk.TaskID
	select {
	case task := <-woke:
		mt = task.ID
		if task.Status.State != sdk.TaskStateCompleted || meta(task)["state"] != string(adapter.Accepted) {
			t.Fatalf("woken %s %v", task.Status.State, meta(task))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve did not wake the default-wait caller")
	}
	if err := e.srv.Resolve(m.ID, adapter.Unreachable, "late"); err != nil {
		t.Fatal(err)
	}

	f := textMsg("lost", nil)
	ft, err := send(t, cl, ctx, f, true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, cl, ctx, ft.ID, sdk.TaskStateWorking)
	if err := e.srv.Resolve(f.ID, adapter.Unreachable, ""); err != nil {
		t.Fatal(err)
	}
	failed := waitState(t, cl, ctx, ft.ID, sdk.TaskStateFailed)
	if meta(failed)["reason"] != string(adapter.Unreachable) {
		t.Fatalf("failed meta %v", meta(failed))
	}

	a := textMsg("which?", map[string]any{"kind": "ask"})
	at, err := send(t, cl, ctx, a, true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, cl, ctx, at.ID, sdk.TaskStateWorking)
	if err := e.srv.Resolve(a.ID, adapter.Accepted, ""); err != nil {
		t.Fatal(err)
	}
	if got := waitState(t, cl, ctx, at.ID, sdk.TaskStateWorking); meta(got)["state"] != string(adapter.Accepted) {
		t.Fatalf("ask meta %v", meta(got))
	}

	if err := e.srv.Resolve(msg.NewUUIDv7(), adapter.Accepted, ""); !errors.Is(err, ErrNoSuchTask) {
		t.Fatalf("unknown message: %v", err)
	}
	if got := e.srv.PendingMessageIDs(); len(got) != 1 || got[0] != a.ID {
		t.Fatalf("pending %v", got)
	}
	e.stop()

	e2 := newEnvOn(t, dir, func(*journal.Journal) TaskStore { return ts }, receiver.Limits{}, Limits{}, nil)
	cl2, ctx2 := e2.peer()
	for tid, want := range map[sdk.TaskID]sdk.TaskState{mt: sdk.TaskStateCompleted, ft.ID: sdk.TaskStateFailed, at.ID: sdk.TaskStateWorking} {
		if got, err := cl2.GetTask(ctx2, &sdk.GetTaskRequest{ID: tid}); err != nil || got.Status.State != want {
			t.Fatalf("%s after restart %v %v, want %s", tid, got, err, want)
		}
	}
}
