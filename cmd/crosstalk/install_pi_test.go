package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness/pi"
	"github.com/GoodOlClint/crosstalk/internal/upgrade"
)

// writeFakePi puts a pi on the sandboxed PATH.
func writeFakePi(t *testing.T) {
	t.Helper()
	dir := os.Getenv("PATH")
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Fatalf("PATH %q is not a test directory", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte("#!/bin/sh\necho 1.6.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// guardPiPaths fails the test unless every Pi path resolves under home.
func guardPiPaths(t *testing.T, home string) {
	t.Helper()
	if !strings.HasPrefix(home, os.TempDir()) || os.Getenv("HOME") != home && !strings.HasPrefix(os.Getenv("HOME"), os.TempDir()) {
		t.Fatalf("home %q is not a test directory", home)
	}
	if d := pi.AgentDir(home); !strings.HasPrefix(d, home+string(os.PathSeparator)) {
		t.Fatalf("Pi agent dir %q is outside the test home %q", d, home)
	}
}

func piSandbox(t *testing.T) (home, bin, ext string) {
	t.Helper()
	home = installSandbox(t)
	writeFakePi(t)
	t.Chdir(t.TempDir())
	guardPiPaths(t, home)
	return home, filepath.Join(home, "bin"), filepath.Join(home, ".pi", "agent", "extensions", "crosstalk", "index.ts")
}

func TestInstallPiEndToEnd(t *testing.T) {
	home, bin, ext := piSandbox(t)
	run := func(want int, args ...string) string {
		t.Helper()
		guardPiPaths(t, home)
		code, out := installRunCmd(t, args[0], append(args[1:], "--bin-dir", bin)...)
		if code != want {
			t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, out)
		}
		return out
	}
	run(0, "install", "pi", "--yes")
	first, err := os.ReadFile(ext)
	if err != nil || !strings.Contains(string(first), filepath.Join(bin, "crosstalk")) {
		t.Fatalf("extension: %v\n%s", err, first)
	}
	run(0, "install", "pi", "--yes")
	if second, _ := os.ReadFile(ext); string(second) != string(first) {
		t.Error("a second install changed the file")
	}
	out := run(0, "install", "--check")
	for _, id := range []string{"K2", "K5", "K6", "K14"} {
		if l := installCheckLine(out, id, "pi"); !strings.Contains(l, "PASS") {
			t.Errorf("%s: %q\n%s", id, l, out)
		}
	}
	run(0, "reinstall", "pi", "--yes", "--no-auto-refresh")
	if b, _ := os.ReadFile(ext); !strings.Contains(string(b), "noRefresh: boolean = true") {
		t.Error("reinstall --no-auto-refresh did not reach the extension")
	}
	run(0, "uninstall", "pi", "--yes")
	if _, err := os.Stat(filepath.Dir(ext)); !os.IsNotExist(err) {
		t.Errorf("uninstall left the extension directory: %v", err)
	}
}

func TestInstallPiRefusalWritesNoPiFile(t *testing.T) {
	home, bin, ext := piSandbox(t)
	cfg := filepath.Join(home, ".pi", "agent", "mcp.json")
	os.MkdirAll(filepath.Dir(cfg), 0o700)
	os.WriteFile(cfg, []byte(`{"mcpServers":{"crosstalk":{}}}`), 0o600)
	code, out := installRunCmd(t, "install", "pi", "--yes", "--bin-dir", bin)
	if code == 0 || !strings.Contains(out, "pi mcp remove crosstalk") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(ext))); !os.IsNotExist(err) {
		t.Errorf("a refused install wrote a Pi file: %v", err)
	}
}

// piFollowUp is an older extension, as an upgrade finds it, under dir.
func piFollowUp(t *testing.T, bin, dir string) string {
	t.Helper()
	ext := filepath.Join(dir, "extensions", "crosstalk", "index.ts")
	if err := os.MkdirAll(filepath.Dir(ext), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ext, pi.Fill(bin, "0.0.1", true, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	return ext
}

func TestFollowUpRewritesPiExtensionPerAgentDir(t *testing.T) {
	_, env, hs := fuSandbox(t, "")
	writeFakePi(t)
	guardPiPaths(t, env.Home)
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(env.Bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := upgrade.MarkDone(env.StateDir, "0.0.1", "pi"); err != nil {
		t.Fatal(err)
	}
	want := string(pi.Fill(env.Bin, env.Version, true, nil))
	for _, profile := range []string{"", "a", "b"} {
		dir := filepath.Join(env.Home, ".pi", "agent")
		if profile != "" {
			dir = filepath.Join(env.Home, profile)
			t.Setenv("PI_CODING_AGENT_DIR", dir)
		}
		guardPiPaths(t, env.Home)
		ext := piFollowUp(t, env.Bin, dir)
		fuRun(env, hs, nil)
		if got, _ := os.ReadFile(ext); string(got) != want {
			t.Errorf("profile %q: not rewritten to %s with no-auto-refresh kept:\n%s", profile, env.Version, got)
		}
		if !fuDone(env, markerKey("pi", env.Home)) {
			t.Errorf("profile %q: no marker %s", profile, markerKey("pi", env.Home))
		}
	}
}

func TestMarkerKeyPi(t *testing.T) {
	home := t.TempDir()
	for _, v := range []string{"", filepath.Join(home, ".pi", "agent"), "~/.pi/agent", filepath.Join(home, ".pi", "agent") + "/"} {
		t.Setenv("PI_CODING_AGENT_DIR", v)
		if k := markerKey("pi", home); k != "pi" {
			t.Errorf("default dir as %q: key %q", v, k)
		}
	}
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "a"))
	a := markerKey("pi", home)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "b"))
	if b := markerKey("pi", home); a == b || !strings.HasPrefix(a, "pi-") || !strings.HasPrefix(b, "pi-") {
		t.Errorf("keys a=%q b=%q", a, b)
	}
}
