package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallPurgeKeepsStateForOtherBinary(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	_, config, state := installUserFiles(t, home)
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	installWrite(t, config, "[mcp_servers.crosstalk]\ncommand = \"/other/crosstalk\"\nargs = [\"mcp\"]\n")
	code, out := installRunCmd(t, "uninstall", "claude", "--purge", "--yes", "--bin-dir", bin)
	if code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("state dir removed while codex runs another binary: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kept "+state) || !strings.Contains(out, "codex") {
		t.Errorf("output does not say why the state stays:\n%s", out)
	}
}

func TestUninstallPurgeKeepsStateForClaudePluginOffPath(t *testing.T) {
	home := installSandbox(t, "codex")
	_, _, state := installUserFiles(t, home)
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	installWrite(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), `{"version":2,"plugins":{"crosstalk@crosstalk":[{"scope":"user"}]}}`)
	code, out := installRunCmd(t, "uninstall", "codex", "--purge", "--yes", "--bin-dir", bin)
	if code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("state dir removed while the claude plugin is installed: %v\n%s", err, out)
	}
}

func TestUninstallPurgeKeepsStateOnCodexCacheStatError(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	_, config, state := installUserFiles(t, home)
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	installWrite(t, config, "model = \"x\"\n")
	cache := filepath.Join(home, ".codex", "plugins", "cache", "crosstalk")
	os.RemoveAll(cache)
	installWrite(t, cache, "")
	code, out := installRunCmd(t, "uninstall", "claude", "--purge", "--yes", "--bin-dir", bin)
	if code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("state dir removed although codex setup could not be read: %v\n%s", err, out)
	}
}
