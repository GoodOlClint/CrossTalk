package appservertest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
)

func TestToolOutputRequestValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		params string
	}{
		{"missing input", "turn/start", `{"threadId":"t","toolOutput":{"name":"receive_message","namespace":"crosstalk","output":"x"}}`},
		{"null input", "turn/start", `{"threadId":"t","input":null,"toolOutput":{"name":"receive_message","namespace":"crosstalk","output":"x"}}`},
		{"user input mixed with output", "turn/start", `{"threadId":"t","input":[{"type":"text","text":"x"}],"toolOutput":{"name":"receive_message","namespace":"crosstalk","output":"x"}}`},
		{"unnamed output", "turn/start", `{"threadId":"t","input":[],"toolOutput":{"namespace":"crosstalk","output":"x"}}`},
		{"empty items", "thread/inject_items", `{"threadId":"t","items":[]}`},
		{"user input item", "thread/inject_items", `{"threadId":"t","items":[{"type":"text","name":"receive_message","namespace":"crosstalk","output":"x"}]}`},
		{"unnamed item", "thread/inject_items", `{"threadId":"t","items":[{"type":"function_call_output","namespace":"crosstalk","output":"x"}]}`},
		{"invalid second item", "thread/inject_items", `{"threadId":"t","items":[{"type":"function_call_output","name":"receive_message","namespace":"crosstalk","output":"x"},{"type":"text"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := appservertest.New(t)
			s.AddThread(&appservertest.Thread{ID: "t", Loaded: true})
			c, ctx := client(t, s)
			_, err := c.CallRaw(ctx, tc.method, json.RawMessage(tc.params))
			var rpc *appserver.RPCError
			if !errors.As(err, &rpc) || rpc.Code != appservertest.CodeInvalidRequest {
				t.Fatalf("invalid request error = %v", err)
			}
			th := s.Thread("t")
			if len(th.ToolOutputs)+len(th.Injected)+len(th.Started)+len(th.Turns) != 0 {
				t.Fatalf("invalid request changed thread: %+v", th)
			}
		})
	}
}

func TestToolOutputRequiresLoadedAcceptingThread(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		for _, immediate := range []bool{false, true} {
			s := appservertest.New(t)
			accept := false
			s.AddThread(&appservertest.Thread{ID: "t", Loaded: loaded, CanAcceptDirectInput: &accept})
			c, ctx := client(t, s)
			var err error
			if immediate {
				_, err = c.StartToolOutput(ctx, "t", "x")
			} else {
				_, err = c.InjectToolOutput(ctx, "t", "x")
			}
			var rpc *appserver.RPCError
			if !errors.As(err, &rpc) || rpc.Code != appservertest.CodeInvalidRequest {
				t.Fatalf("loaded=%v immediate=%v: %v", loaded, immediate, err)
			}
			th := s.Thread("t")
			if len(th.ToolOutputs)+len(th.Injected)+len(th.Turns) != 0 {
				t.Fatalf("unreachable thread changed: %+v", th)
			}
		}
	}
}

func TestThreadSnapshotClonesToolOutputs(t *testing.T) {
	s := appservertest.New(t)
	s.AddThread(&appservertest.Thread{ID: "t", Loaded: true})
	c, ctx := client(t, s)
	if _, err := c.StartToolOutput(ctx, "t", "immediate"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InjectToolOutput(ctx, "t", "deferred"); err != nil {
		t.Fatal(err)
	}
	copy := s.Thread("t")
	copy.ToolOutputs[0].Output = "changed"
	copy.Injected[0].Output = "changed"
	got := s.Thread("t")
	if got.ToolOutputs[0].Output != "immediate" || got.Injected[0].Output != "deferred" {
		t.Fatalf("snapshot changed server: %+v", got)
	}
}

func client(t *testing.T, s *appservertest.Server) (*appserver.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, err := appserver.Dial(ctx, s.Endpoint(), appserver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Initialize(ctx, appserver.ClientInfo{Name: "crosstalk", Version: "test"}); err != nil {
		t.Fatal(err)
	}
	return c, ctx
}
