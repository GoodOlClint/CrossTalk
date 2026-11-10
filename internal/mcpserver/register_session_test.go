package mcpserver

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

func TestRegisterSession_ThenToolsCallSameThread_ExpectOneAgent(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	ctx := context.Background()
	req := harness.RequestContext{Meta: thread("T-claimed")}
	if err := e.srv.RegisterSession(ctx, req, "/work/claimed"); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.RegisterSession(ctx, req, "/work/claimed"); err != nil {
		t.Fatalf("refire: %v", err)
	}
	d := e.descriptor(codexID("T-claimed"))
	if d.Cwd != "/work/claimed" {
		t.Fatalf("descriptor %+v", d)
	}
	e.mustOK(thread("T-claimed"), "send_message", sendArgs("peer", "hi"))
	calls := e.fb.Calls()
	if len(calls) != 1 || calls[0].From != codexID("T-claimed") {
		t.Fatalf("calls %+v", calls)
	}
	ds, _ := e.reg.List()
	if len(ds) != 1 {
		t.Fatalf("descriptors %+v, want one", ds)
	}
}

func TestRegisterSession_NoHarnessClaims_ExpectUnboundNoDescriptor(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	err := e.srv.RegisterSession(context.Background(), harness.RequestContext{}, "")
	var me *Error
	if !errors.As(err, &me) || me.Code != CodeUnbound {
		t.Fatalf("err %v", err)
	}
	if ds, _ := e.reg.List(); len(ds) != 0 {
		t.Fatalf("descriptors %+v", ds)
	}
}

func TestRegisterSession_AgentHeldByAnotherLiveShim_ExpectRejectedAndRecordUntouched(t *testing.T) {
	e1 := newEnv(t, 999999, NewFakeBackend())
	e1.mustOK(thread("T-held"), "list_agents", map[string]any{})
	before := e1.descriptor(codexID("T-held"))

	sd, err := os.MkdirTemp("", "ctsess")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sd) })
	e2 := &env{t: t, stateDir: e1.stateDir, sessionDir: sd, fb: NewFakeBackend()}
	e2.start(999998)
	err = e2.srv.RegisterSession(context.Background(), harness.RequestContext{Meta: thread("T-held")}, "/elsewhere")
	var me *Error
	if !errors.As(err, &me) || me.Reason != "agent_held" {
		t.Fatalf("err %v, want agent_held", err)
	}
	after := e1.descriptor(codexID("T-held"))
	if after.InstanceID != before.InstanceID || after.Cwd != before.Cwd {
		t.Fatalf("existing record changed: %+v -> %+v", before, after)
	}
}
