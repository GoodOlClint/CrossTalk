package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installBuild builds pkg (relative to this package) into a temp dir.
func installBuild(t *testing.T, pkg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "crosstalk")
	b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
	return out
}

func TestCheckHandshake(t *testing.T) {
	real := installBuild(t, ".")
	fake := installBuild(t, "./testdata/installfakemcp")
	home := t.TempDir()
	env := []string{"HOME=" + home}
	if err := installHandshake(context.Background(), real, []string{"mcp"}, env); err != nil {
		t.Errorf("real binary: %v", err)
	}
	var files []string
	filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) > 0 {
		t.Errorf("handshake wrote files: %v", files)
	}
	if err := installHandshake(context.Background(), fake, nil, env); err == nil || !strings.Contains(err.Error(), "0.0.0-fake") {
		t.Errorf("wrong version: %v", err)
	}
	if err := installHandshake(context.Background(), real, []string{"crosstalk", "mcp"}, env); err == nil {
		t.Error("wrapper args kept: want a failure")
	}
}

func installCheckLine(out, id, harness string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, id+" "+harness+" ") {
			return l
		}
	}
	return ""
}

func TestCheckOrphanToolsTable(t *testing.T) {
	home := installSandbox(t, "codex")
	cfg := filepath.Join(home, ".codex", "config.toml")
	os.MkdirAll(filepath.Dir(cfg), 0o700)
	os.WriteFile(cfg, []byte("[mcp_servers.crosstalk.tools.ask]\napproval_mode = \"prompt\"\n"), 0o600)
	code, out := installRunCmd(t, "install", "--check", "--bin-dir", filepath.Join(home, "bin"))
	if code != 1 || !strings.Contains(installCheckLine(out, "K3", "codex"), "FAIL") {
		t.Errorf("orphan: exit %d\n%s", code, out)
	}
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin")); code != 0 || !strings.Contains(installCheckLine(out, "K3", "codex"), "PASS") {
		t.Errorf("after install: exit %d\n%s", code, out)
	}
}

func TestCheckTwoServers(t *testing.T) {
	home := installSandbox(t, "claude")
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 || !strings.Contains(installCheckLine(out, "K2", "claude"), "PASS") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	b, _ := json.Marshal([]map[string]any{{"id": "crosstalk@crosstalk", "version": "0.1.1", "mcpServers": map[string]any{"crosstalk": map[string]any{"command": "crosstalk"}}}})
	os.WriteFile(filepath.Join(home, "fake-claude-plugins.json"), b, 0o600)
	code, out := installRunCmd(t, "install", "--check", "--bin-dir", bin)
	if code != 1 || !strings.Contains(installCheckLine(out, "K2", "claude"), "FAIL") || !strings.Contains(installCheckLine(out, "K1", "claude"), "FAIL") {
		t.Errorf("plugin server: exit %d\n%s", code, out)
	}
}

func TestInstallChecksPass(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin"))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, h := range []string{"claude", "codex"} {
		for _, id := range []string{"K1", "K2", "K5", "K6"} {
			if l := installCheckLine(out, id, h); !strings.Contains(l, "PASS") {
				t.Errorf("%s %s: %q", id, h, l)
			}
		}
	}
	for _, c := range []struct{ id, h string }{{"K3", "codex"}, {"K4", "claude"}} {
		if l := installCheckLine(out, c.id, c.h); !strings.Contains(l, "PASS") {
			t.Errorf("%s %s: %q", c.id, c.h, l)
		}
	}
	if l := installCheckLine(out, "K7", "crosstalk"); !strings.Contains(l, "PASS") && !strings.Contains(l, "WARN") {
		t.Errorf("K7: %q", l)
	}
	if !strings.Contains(out, "Restart them") {
		t.Error("no restart notice")
	}
}
