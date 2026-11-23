package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/upgrade"
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
	if !installStatePurged(state) {
		t.Errorf("state kept for a .mcp.json read through an empty or relative project key:\n%s", out)
	}
}

func TestUninstallRemovesOnlyItsHarnessMarkers(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if !upgrade.Done(state, mcpserver.Version, "claude") || !upgrade.Done(state, mcpserver.Version, "codex") {
		t.Fatal("install did not write both markers")
	}
	if err := upgrade.MarkDone(state, "0.0.1", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := upgrade.MarkDone(state, mcpserver.Version, "gateway"); err != nil {
		t.Fatal(err)
	}
	installWrite(t, upgrade.LogPath(state, mcpserver.Version), "log\n")
	if code, out := installRunCmd(t, "uninstall", "claude", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	for _, v := range []string{mcpserver.Version, "0.0.1"} {
		if upgrade.Done(state, v, "claude") {
			t.Errorf("claude marker for %s survived uninstall claude", v)
		}
	}
	if !upgrade.Done(state, mcpserver.Version, "codex") {
		t.Error("uninstall claude removed the codex marker")
	}
	if !upgrade.Done(state, mcpserver.Version, "gateway") {
		t.Error("uninstall claude removed the gateway marker")
	}
	if installRead(t, upgrade.LogPath(state, mcpserver.Version)) != "log\n" {
		t.Error("uninstall claude touched the follow-up log")
	}
}

func TestPurgeKeepsHeldUpgradeLock(t *testing.T) {
	state := t.TempDir()
	rel, ok, err := upgrade.TryLock(state)
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	defer rel()
	installWrite(t, filepath.Join(state, "journal", "a.jsonl"), "x")
	installWrite(t, filepath.Join(state, "upgrade", "1", "claude.done"), "x")
	installWrite(t, upgrade.ChildPath(state), "x")
	if err := purgeState(state); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := upgrade.TryLock(state); ok {
		t.Error("a second process took upgrade.lock while a holder kept it open across purge")
	}
	es, _ := os.ReadDir(state)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "upgrade.lock,upgrade.lock.child" {
		t.Errorf("purge left %s", got)
	}
}

func installStatePurged(state string) bool {
	es, err := os.ReadDir(state)
	if err != nil {
		return os.IsNotExist(err)
	}
	for _, e := range es {
		if e.Name() != "upgrade.lock" && e.Name() != "upgrade.lock.child" {
			return false
		}
	}
	return true
}

func installWithoutMarkers(m map[string]string) string {
	for k := range m {
		if b := filepath.Base(k); b == "claude.done" || b == "codex.done" {
			delete(m, k)
		}
	}
	return fmt.Sprint(m)
}

func TestPurgeStateSymlinkRemovesOnlyTheLink(t *testing.T) {
	target := t.TempDir()
	installWrite(t, filepath.Join(target, "shared", "f"), "keep")
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := purgeState(link); err != nil {
		t.Fatal(err)
	}
	if installRead(t, filepath.Join(target, "shared", "f")) != "keep" {
		t.Error("purge followed the state symlink")
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("link kept: %v", err)
	}
}
