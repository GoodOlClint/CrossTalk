package shim

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/opencode"
	"github.com/GoodOlClint/crosstalk/internal/harness/opencode/opencodetest"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/prebind"
)

func TestOpenCodeNativeSendKeepsPreBindReceiptOnlyWhenAdmitted(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.token(alice.agentID())
	srv := opencodetest.Start(t,
		opencodetest.Session{ID: "ses_acc1", Title: "oc-acc", Updated: time.Now()},
		opencodetest.Session{ID: "ses_que1", Title: "oc-que", Updated: time.Now()},
		opencodetest.Session{ID: "ses_unk1", Title: "oc-unk", Updated: time.Now()})
	svc := opencodetest.WriteService(t, t.TempDir(), srv.URL, "2.4.0", os.Getpid())
	na := w.node(alice.pid, nil, func(o *Options) {
		o.Harnesses = append(o.Harnesses, harness.Harness(opencode.NewHarness(opencode.Options{ServicePath: svc})))
	})

	acc := na.ok(nil, "send_message", send("oc-acc", "fyi"))
	que := na.ok(nil, "send_message", map[string]any{"to": "oc-que", "text": "later", "delivery": "next_turn"})
	if acc["state"] != "accepted" || que["state"] != "queued" {
		t.Fatalf("states %v %v, want accepted and queued", acc["state"], que["state"])
	}
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
	for ref, res := range map[string]map[string]any{"ses_acc1": acc, "ses_que1": que} {
		b, err := os.ReadFile(filepath.Join(w.state, "prebind", identity.AgentID("opencode", ref)+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var e prebind.Entry
		if err := json.Unmarshal(bytes.TrimSpace(b), &e); err != nil || !strings.Contains(e.Message, res["message_id"].(string)) {
			t.Fatalf("%s receipt %+v %v, want message_id %v", ref, e, err, res["message_id"])
		}
	}
}
