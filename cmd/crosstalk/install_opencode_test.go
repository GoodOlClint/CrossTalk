package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const openCodeCommented = "{\n  // a user comment\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"theme\": \"x\", /* inline */\n}\n"

func openCodeSandbox(t *testing.T) (home, bin, cfg string) {
	t.Helper()
	home = installSandbox(t, "opencode")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	bin = filepath.Join(home, "bin")
	cfg = filepath.Join(home, ".config", "opencode", "opencode.json")
	return home, bin, cfg
}

func TestInstallOpenCodeEndToEnd(t *testing.T) {
	t.Run("commented config", func(t *testing.T) {
		_, bin, cfg := openCodeSandbox(t)
		if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, []byte(openCodeCommented), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, out := installRunCmd(t, "install", "opencode", "--yes", "--bin-dir", bin); code != 0 {
			t.Fatalf("install: exit %d\n%s", code, out)
		}
		got, _ := os.ReadFile(cfg)
		for _, want := range []string{"// a user comment", "/* inline */", filepath.Join(bin, "crosstalk")} {
			if !strings.Contains(string(got), want) {
				t.Errorf("config lacks %q:\n%s", want, got)
			}
		}
		code, out := installRunCmd(t, "install", "--check", "opencode", "--bin-dir", bin)
		if code != 0 {
			t.Errorf("--check: exit %d\n%s", code, out)
		}
		for _, id := range []string{"K2", "K5", "K6", "K13"} {
			if l := installCheckLine(out, id, "opencode"); !strings.Contains(l, "PASS") {
				t.Errorf("%s: %q\n%s", id, l, out)
			}
		}
		code, out = installRunCmd(t, "uninstall", "opencode", "--yes", "--bin-dir", bin)
		if code == 0 || !strings.Contains(out, "comments") || !strings.Contains(out, `"crosstalk"`) {
			t.Errorf("uninstall of a commented config: exit %d\n%s", code, out)
		}
		if after, _ := os.ReadFile(cfg); string(after) != string(got) {
			t.Errorf("uninstall changed a commented config:\n%s", after)
		}
	})

	t.Run("plain config", func(t *testing.T) {
		_, bin, cfg := openCodeSandbox(t)
		if code, out := installRunCmd(t, "install", "opencode", "--yes", "--no-auto-refresh", "--bin-dir", bin); code != 0 {
			t.Fatalf("install: exit %d\n%s", code, out)
		}
		if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), "CROSSTALK_NO_AUTO_REFRESH") {
			t.Errorf("no environment:\n%s", b)
		}
		if code, out := installRunCmd(t, "install", "--check", "opencode", "--bin-dir", bin); code != 0 || !strings.Contains(installCheckLine(out, "K6", "opencode"), "PASS") {
			t.Errorf("--check: exit %d\n%s", code, out)
		}
		if code, out := installRunCmd(t, "uninstall", "opencode", "--yes", "--bin-dir", bin); code != 0 {
			t.Fatalf("uninstall: exit %d\n%s", code, out)
		}
		if b, _ := os.ReadFile(cfg); strings.Contains(string(b), "crosstalk") {
			t.Errorf("entry left behind:\n%s", b)
		}
	})
}
