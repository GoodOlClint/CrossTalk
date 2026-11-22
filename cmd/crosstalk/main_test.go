package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

func TestRun(t *testing.T) {
	cases := []struct {
		args []string
		code int
		out  string
		err  string
	}{
		{nil, 2, "", "usage: crosstalk"},
		{[]string{"help"}, 0, "usage: crosstalk", ""},
		{[]string{"capture"}, 2, "", "usage: crosstalk capture"},
		{[]string{"bogus"}, 2, "", "unknown command"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if got := run(c.args, &out, &errb); got != c.code {
			t.Errorf("run(%v) = %d, want %d", c.args, got, c.code)
		}
		if !strings.Contains(out.String(), c.out) || !strings.Contains(errb.String(), c.err) {
			t.Errorf("run(%v) stdout=%q stderr=%q", c.args, out.String(), errb.String())
		}
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("CROSSTALK_TEST_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	code := m.Run()
	if builtMain.dir != "" {
		os.RemoveAll(builtMain.dir)
	}
	os.Exit(code)
}

func TestMCPExitsCleanlyOnClosedStdin(t *testing.T) {
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "mcp")
	cmd.Env = append(os.Environ(), "CROSSTALK_TEST_MAIN=1", "HOME="+home)
	cmd.Stdin = strings.NewReader("")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("crosstalk mcp: %v, stderr %q", err, errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "crosstalk", "agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("protocol start created agent state: %v", err)
	}
}

func TestCodexEndpointsFromEnv(t *testing.T) {
	t.Setenv("CROSSTALK_CODEX_ENDPOINTS", "unix:///tmp/a.sock, ws://127.0.0.1:4500 ,unix:///tmp/b.sock")
	hs, err := harnesses(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ad := hs[0].(*codex.Harness).Threads.(*codex.Adapter)
	got := ad.KnownEndpoints()
	for _, want := range []string{"unix:///tmp/a.sock", "ws://127.0.0.1:4500", "unix:///tmp/b.sock"} {
		if !slices.Contains(got, want) {
			t.Fatalf("KnownEndpoints %v lacks %s", got, want)
		}
	}
}

func TestVersionJSONReportsRefreshHandoff(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var got struct {
		Version        string `json:"version"`
		RefreshHandoff int    `json:"refresh_handoff"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %q: %v", out.String(), err)
	}
	if got.Version != mcpserver.Version || got.RefreshHandoff != mcpserver.HandoffFormat || got.RefreshHandoff < 1 {
		t.Fatalf("version --json = %+v", got)
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errb); code != 0 || out.String() != mcpserver.Version+"\n" {
		t.Fatalf("plain version = %d %q", code, out.String())
	}
}
