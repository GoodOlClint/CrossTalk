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

func TestUninstallPurgeKeepsStateForClaudeNonUserScope(t *testing.T) {
	cases := map[string]func(t *testing.T, home, bin string){
		"local": func(t *testing.T, home, bin string) {
			installWrite(t, filepath.Join(home, ".claude.json"), `{"projects":{"`+filepath.Join(home, "proj")+`":{"mcpServers":{"crosstalk":{"command":"`+filepath.Join(bin, "crosstalk")+`","args":["mcp"]}}}}}`)
		},
		"project": func(t *testing.T, home, bin string) {
			proj := filepath.Join(home, "proj")
			installWrite(t, filepath.Join(home, ".claude.json"), `{"projects":{"`+proj+`":{}}}`)
			installWrite(t, filepath.Join(proj, ".mcp.json"), `{"mcpServers":{"crosstalk":{"command":"`+filepath.Join(bin, "crosstalk")+`","args":["mcp"]}}}`)
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			home := installSandbox(t, "codex")
			_, _, state := installUserFiles(t, home)
			bin := filepath.Join(home, "bin")
			if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
				t.Fatalf("install: %d\n%s", code, out)
			}
			seed(t, home, bin)
			code, out := installRunCmd(t, "uninstall", "codex", "--purge", "--yes", "--bin-dir", bin)
			if code != 0 {
				t.Fatalf("uninstall: %d\n%s", code, out)
			}
			if _, err := os.Stat(state); err != nil {
				t.Errorf("state dir removed while claude has a %s-scope entry: %v\n%s", name, err, out)
			}
			if _, err := os.Stat(filepath.Join(bin, "crosstalk")); err != nil {
				t.Errorf("binary removed while claude has a %s-scope entry: %v\n%s", name, err, out)
			}
		})
	}
}

func TestUninstallPurgeKeepsStateForClaudeLocalEntryWhenClaudeSelected(t *testing.T) {
	for _, sel := range [][]string{{"claude"}, {}} {
		t.Run(strings.Join(sel, "")+"-all", func(t *testing.T) {
			home := installSandbox(t, "claude")
			_, _, state := installUserFiles(t, home)
			bin := filepath.Join(home, "bin")
			if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
				t.Fatalf("install: %d\n%s", code, out)
			}
			cj := filepath.Join(home, ".claude.json")
			b, err := os.ReadFile(cj)
			if err != nil {
				t.Fatal(err)
			}
			installWrite(t, cj, strings.Replace(string(b), "{", `{"projects":{"`+filepath.Join(home, "proj")+`":{"mcpServers":{"crosstalk":{"command":"`+filepath.Join(bin, "crosstalk")+`","args":["mcp"]}}}},`, 1))
			args := append(sel, "--purge", "--yes", "--bin-dir", bin)
			if code, out := installRunCmd(t, "uninstall", args...); code != 0 {
				t.Fatalf("%v: %d\n%s", args, code, out)
			}
			if _, err := os.Stat(state); err != nil {
				t.Errorf("%v: state removed while a local-scope entry remains: %v", args, err)
			}
			if _, err := os.Stat(filepath.Join(bin, "crosstalk")); err != nil {
				t.Errorf("%v: binary removed while a local-scope entry remains: %v", args, err)
			}
		})
	}
}

func TestUninstallPurgeBinaryIgnoresLocalEntryForOtherBinary(t *testing.T) {
	for _, own := range []bool{false, true} {
		name := "other"
		if own {
			name = "own"
		}
		t.Run(name, func(t *testing.T) {
			home := installSandbox(t, "claude")
			_, _, state := installUserFiles(t, home)
			bin := filepath.Join(home, "bin")
			if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
				t.Fatalf("install: %d\n%s", code, out)
			}
			cmd := "/other/crosstalk"
			if own {
				cmd = filepath.Join(bin, "crosstalk")
			}
			installWrite(t, filepath.Join(home, ".claude.json"), `{"projects":{"`+filepath.Join(home, "proj")+`":{"mcpServers":{"crosstalk":{"command":"`+cmd+`","args":["mcp"]}}}}}`)
			code, out := installRunCmd(t, "uninstall", "claude", "--purge", "--yes", "--bin-dir", bin)
			if code != 0 {
				t.Fatalf("uninstall: %d\n%s", code, out)
			}
			if _, err := os.Stat(state); err != nil {
				t.Errorf("state removed while claude has a local-scope entry: %v\n%s", err, out)
			}
			_, err := os.Stat(filepath.Join(bin, "crosstalk"))
			if own && err != nil {
				t.Errorf("binary removed while a local-scope entry runs it: %v\n%s", err, out)
			}
			if !own && err == nil {
				t.Errorf("binary kept for an entry that names another binary:\n%s", out)
			}
			if !strings.Contains(out, "local scope, project "+filepath.Join(home, "proj")+" runs "+cmd) {
				t.Errorf("kept message does not name scope and path:\n%s", out)
			}
		})
	}
}

func TestUninstallPurgeSkipsEmptyProjectKey(t *testing.T) {
	home := installSandbox(t, "codex")
	_, _, state := installUserFiles(t, home)
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	cwd := t.TempDir()
	installWrite(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"crosstalk":{"command":"/other/crosstalk","args":["mcp"]}}}`)
	t.Chdir(cwd)
	installWrite(t, filepath.Join(home, ".claude.json"), `{"projects":{"":{},"rel":{}}}`)
	code, out := installRunCmd(t, "uninstall", "codex", "--purge", "--yes", "--bin-dir", bin)
	if code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	if _, err := os.Stat(state); err == nil {
		t.Errorf("state kept for a .mcp.json read through an empty or relative project key:\n%s", out)
	}
}
