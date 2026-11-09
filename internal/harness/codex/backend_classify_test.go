package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

func TestClassifyBackend(t *testing.T) {
	raw, err := os.ReadFile("testdata/ratelimits-backend-down.json")
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Error *appserver.RPCError `json:"error"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || frame.Error == nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, "reachable"},
		{"measured outage frame", frame.Error, "unreachable"},
		{"wrapped outage", fmt.Errorf("probe: %w", frame.Error), "unreachable"},
		{"connection failed", &appserver.RPCError{Code: -32603, Message: "Connection failed: refused"}, "unreachable"},
		{"invalid request", &appserver.RPCError{Code: -32600, Message: "invalid request"}, "unknown"},
		{"other internal error", &appserver.RPCError{Code: -32603, Message: "failed to fetch codex rate limits: unauthorized"}, "unknown"},
		{"ambiguous", appserver.ErrAmbiguous, "unknown"},
		{"deadline", context.DeadlineExceeded, "unknown"},
		{"canceled", context.Canceled, "unknown"},
		{"plain error", errors.New("boom"), "unknown"},
	} {
		if got := classifyBackend(c.err); got != c.want {
			t.Errorf("%s: classifyBackend = %q, want %q", c.name, got, c.want)
		}
	}
}
