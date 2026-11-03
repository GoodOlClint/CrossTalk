package codex_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
)

var update = flag.Bool("update", false, "regenerate testdata/codex/fake-0.166.0 fixtures")

const fakeFixtures = "testdata/codex/fake-0.166.0"

type scenario struct {
	name  string
	th    func() *appservertest.Thread
	steer string
}

var scenarios = []scenario{
	{"steerable-active", func() *appservertest.Thread { return activeThread("019a0000-0000-7000-8000-00000000bbbb") }, ""},
	{"idle", idleThread, "capture live steer"},
	{"review", func() *appservertest.Thread {
		th := activeThread("019a0000-0000-7000-8000-00000000bbbb")
		th.NotSteerable = "review"
		return th
	}, "capture live steer"},
	{"compact", func() *appservertest.Thread {
		th := activeThread("019a0000-0000-7000-8000-00000000bbbb")
		th.NotSteerable = "compact"
		return th
	}, "capture live steer"},
	{"not-loaded", func() *appservertest.Thread {
		th := idleThread()
		th.Loaded = false
		return th
	}, "capture live steer"},
}

func runCapture(t *testing.T, sc scenario, dir string) string {
	t.Helper()
	s := appservertest.New(t)
	s.AddThread(sc.th())
	s.Hook = func(_ int, method string, _ json.RawMessage) appservertest.Reply {
		if method != "mcpServerStatus/list" {
			return appservertest.Reply{}
		}
		return appservertest.Reply{Handled: true, Result: map[string]any{"data": []any{map[string]any{"name": "crosstalk", "_meta": map[string]any{"source": "fake"}}}, "nextCursor": nil}}
	}
	path, err := codex.Capture(ctx(t), codex.CaptureOptions{Endpoint: s.Endpoint(), ThreadID: thread, OutDir: dir, SteerText: sc.steer, Listen: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("%s: Capture: %v", sc.name, err)
	}
	return path
}

// The committed fake fixtures are exactly what Capture produces against
// the fake app-server (run with -update to regenerate).
func TestCaptureGoldenAgainstFake(t *testing.T) {
	for _, sc := range scenarios {
		dir := filepath.Join(fakeFixtures, sc.name)
		if !*update {
			dir = t.TempDir()
		}
		got, err := os.ReadFile(runCapture(t, sc, dir))
		if err != nil {
			t.Fatal(err)
		}
		if *update {
			continue
		}
		want, err := os.ReadFile(filepath.Join(fakeFixtures, sc.name, codex.CaptureFile))
		if err != nil {
			t.Fatalf("%s: %v (run go test -update)", sc.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: capture differs from fixture; run go test -update and review the diff", sc.name)
		}
	}
}

type wireFrame struct {
	ID     json.RawMessage     `json:"id"`
	Method string              `json:"method"`
	Result json.RawMessage     `json:"result"`
	Error  *appserver.RPCError `json:"error"`
}

func responses(recs []codex.CaptureRecord, step string) []wireFrame {
	var out []wireFrame
	for _, r := range recs {
		if r.Step != step || r.Kind != codex.CaptureKindFrame || r.Dir != appserver.DirIn {
			continue
		}
		var f wireFrame
		if json.Unmarshal(r.Frame, &f) == nil && len(f.ID) > 0 && f.Method == "" {
			out = append(out, f)
		}
	}
	return out
}

// Every fixture set under testdata/codex/<version>/<scenario>/ (fake and,
// once committed, live captures) drives the steer error classifier: a
// bogus-expectedTurnId steer is never a success, and in review/compact
// scenarios the live steer classifies as activeTurnNotSteerable.
func TestCaptureFixturesDriveClassifier(t *testing.T) {
	files, err := filepath.Glob("testdata/codex/*/*/" + codex.CaptureFile)
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		recs, err := codex.ReadCapture(f)
		if err != nil {
			t.Fatal(err)
		}
		scen := filepath.Base(filepath.Dir(f))
		var steps []string
		sawMeta := false
		for _, r := range recs {
			steps = append(steps, r.Step)
			sawMeta = sawMeta || r.Kind == codex.CaptureKindMeta
			if r.Kind == codex.CaptureKindFrame && r.Dir == appserver.DirOut && !bytes.Contains(r.Frame, []byte(`"jsonrpc":"2.0"`)) {
				t.Errorf("%s: sent frame without jsonrpc header: %s", f, r.Frame)
			}
		}
		for _, step := range []string{codex.StepSteerMismatch, codex.StepSteerNoActive} {
			for _, resp := range responses(recs, step) {
				if resp.Error == nil {
					t.Errorf("%s: %s succeeded: %s", f, step, resp.Result)
				}
			}
		}
		if kind := scen; kind == "review" || kind == "compact" {
			live := responses(recs, codex.StepSteerLive)
			if len(live) != 1 {
				t.Fatalf("%s: %d steer_live responses", f, len(live))
			}
			var err error
			if live[0].Error != nil {
				err = live[0].Error
			}
			got, ok := codex.NotSteerable(live[0].Result, err)
			if !ok || (got != "" && got != kind) {
				t.Errorf("%s: steer_live classified notSteerable=%v kind=%q", f, ok, got)
			}
		}
		if scen == "not-loaded" && (slices.Contains(steps, codex.StepSteerMismatch) || slices.Contains(steps, codex.StepSteerNoActive) || slices.Contains(steps, codex.StepSteerLive)) {
			t.Errorf("%s: steered a thread not loaded on the endpoint", f)
		}
		if strings.HasPrefix(filepath.Base(filepath.Dir(filepath.Dir(f))), "fake-") && !sawMeta {
			t.Errorf("%s: fake mcpServerStatus _meta not recorded", f)
		}
	}
}

func TestCaptureNeverDeliversProbe(t *testing.T) {
	for _, sc := range scenarios {
		s := appservertest.New(t)
		s.AddThread(sc.th())
		if _, err := codex.Capture(ctx(t), codex.CaptureOptions{Endpoint: s.Endpoint(), ThreadID: thread, OutDir: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		th := s.Thread(thread)
		if len(th.Steered) != 0 || len(th.Started) != 0 || len(s.CallsTo("turn/start")) != 0 {
			t.Errorf("%s: capture without SteerText delivered %v %v", sc.name, th.Steered, th.Started)
		}
	}
}
