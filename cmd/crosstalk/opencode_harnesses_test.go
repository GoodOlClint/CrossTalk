package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/opencode/opencodetest"
)

func TestHarnessesListsOpenCodeSession(t *testing.T) {
	srv := opencodetest.Start(t, opencodetest.Session{ID: "ses_listed1", Title: "oc-listed", Dir: "/work/oc", Updated: time.Now()})
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	opencodetest.WriteService(t, filepath.Join(state, "opencode"), srv.URL, "2.4.0", 4242)
	t.Setenv("XDG_STATE_HOME", state)

	hs, err := harnesses(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if h.Name() != adapter.OpenCode {
			continue
		}
		got, err := h.Discover(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].SessionRef != "ses_listed1" || got[0].Name != "oc-listed" {
			t.Fatalf("discovered %+v", got)
		}
		return
	}
	t.Fatalf("harnesses() lacks the OpenCode harness")
}
