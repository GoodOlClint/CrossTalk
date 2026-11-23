package shim

import (
	"slices"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/opencode"
	"github.com/GoodOlClint/crosstalk/internal/harness/opencode/opencodetest"
	"github.com/GoodOlClint/crosstalk/internal/identity"
)

func TestOpenCodeNativeSendKeepsPreBindReceiptOnlyWhenAdmitted(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.token(alice.agentID())
	srv := opencodetest.Start(t,
		opencodetest.Session{ID: "ses_acc1", Title: "oc-acc", Updated: time.Now()},
		opencodetest.Session{ID: "ses_que1", Title: "oc-que", Updated: time.Now()},
		opencodetest.Session{ID: "ses_unk1", Title: "oc-unk", Updated: time.Now()})
	svc := opencodetest.WriteService(t, t.TempDir(), srv.URL, "2.4.0", 4242)
	na := w.node(alice.pid, nil, func(o *Options) {
		o.Harnesses = append(o.Harnesses, harness.Harness(opencode.NewHarness(opencode.Options{ServicePath: svc})))
	})

	na.ok(nil, "send_message", send("oc-acc", "fyi"))
	na.ok(nil, "send_message", map[string]any{"to": "oc-que", "text": "later", "delivery": "next_turn"})
	srv.Set(func(s *opencodetest.Service) { s.Prompt.Status = 500 })
	res := na.ok(nil, "send_message", send("oc-unk", "maybe"))
	if res["state"] != "unknown" {
		t.Fatalf("send %v, want unknown", res)
	}

	got := w.prebindFiles()
	want := []string{identity.AgentID("opencode", "ses_acc1"), identity.AgentID("opencode", "ses_que1")}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("prebind files %v, want %v", got, want)
	}
}
