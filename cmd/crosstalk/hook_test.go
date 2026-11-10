package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
)

func runHookProcess(t *testing.T, home, stdin string) (string, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "hook", "codex", "session-start")
	cmd.Env = append(os.Environ(), "CROSSTALK_TEST_MAIN=1", "HOME="+home)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook exited with %v", err)
	}
	return out.String(), time.Since(start)
}

func TestHookCommand_SettingOffOrOnAnyInput_ExpectEmptyObjectExit0(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".local", "state", "crosstalk")
	payload := `{"session_id":"T1","source":"startup","cwd":"/work"}`
	for _, on := range []bool{false, true} {
		if on {
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, codex.SettingFile), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, in := range []string{"", "garbage", payload} {
			out, took := runHookProcess(t, home, in)
			if out != "{}\n" {
				t.Fatalf("on=%v stdin %q: stdout %q", on, in, out)
			}
			if took > 10*time.Second {
				t.Fatalf("hook took %v", took)
			}
		}
		if _, err := os.Stat(codex.ClaimDir(state)); !on && !os.IsNotExist(err) {
			t.Fatalf("claim dir written with the setting off: %v", err)
		}
	}
}

func TestHookCommand_UnknownHook_ExpectUsageExit2(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"hook", "claude", "x"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "crosstalk hook codex session-start") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}
