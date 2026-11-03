package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
)

func TestCaptureCommand(t *testing.T) {
	s := appservertest.New(t)
	s.AddThread(&appservertest.Thread{ID: "t1", Loaded: true})
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := run([]string{"capture", "-endpoint", s.Endpoint(), "-thread", "t1", "-out", dir, "-listen", "0s"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := filepath.Join(dir, codex.CaptureFile)
	if strings.TrimSpace(out.String()) != want {
		t.Fatalf("stdout %q, want %q", out.String(), want)
	}
	recs, err := codex.ReadCapture(want)
	if err != nil || len(recs) == 0 {
		t.Fatalf("records %d, %v", len(recs), err)
	}
	if len(s.CallsTo("turn/steer")) != 1 || len(s.Thread("t1").Steered) != 0 {
		t.Fatal("capture must send exactly the precondition-failing steer and deliver nothing")
	}
}

func TestCaptureCommandUsage(t *testing.T) {
	for _, args := range [][]string{{"capture"}, {"capture", "-thread", "t1"}, {"capture", "-out", "x"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: crosstalk capture") {
			t.Errorf("run(%v) = %d, stderr %q", args, code, errb.String())
		}
	}
}
