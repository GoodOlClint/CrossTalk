package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func compatDetect(t *testing.T, npmVersion string, args ...string) (string, error) {
	t.Helper()
	root := releaseRepoRoot(t)
	bin := t.TempDir()
	fake := "#!/bin/sh\necho " + npmVersion + "\n"
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "harness-compat.sh"), append([]string{"detect", filepath.Join(root, ".github", "harness-compat.json")}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestHarnessCompatDetect(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	cases := []struct {
		name   string
		npm    string
		args   []string
		want   string
		wantOK bool
	}{
		{"unverified record is changed", "1.2.3", []string{"codex", "", "", ""}, `[{"harness":"codex","version":"1.2.3"}]`, true},
		{"equal to recorded ends", "1.2.3", []string{"codex", "", "", "1.2.3"}, `[]`, true},
		{"force reruns an equal version", "1.2.3", []string{"codex", "", "true", "1.2.3"}, `[{"harness":"codex","version":"1.2.3"}]`, true},
		{"forced version skips npm", "9.9.9", []string{"codex", "0.171.0", "", ""}, `[{"harness":"codex","version":"0.171.0"}]`, true},
		{"all covers both", "1.2.3", []string{"all", "", "", "1.2.3"}, `[]`, true},
		{"forced version needs one harness", "1.2.3", []string{"all", "0.171.0", "", ""}, "", false},
		{"forced version must be semver", "1.2.3", []string{"codex", "1.2.3; rm -rf /", "", ""}, "", false},
		{"npm garbage refused", "latest", []string{"codex", "", "", ""}, "", false},
		{"unknown harness refused", "1.2.3", []string{"cursor", "", "", ""}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := compatDetect(t, c.npm, c.args...)
			if (err == nil) != c.wantOK {
				t.Fatalf("err = %v, want ok=%v\n%s", err, c.wantOK, out)
			}
			if c.wantOK && out != c.want {
				t.Fatalf("got %s, want %s", out, c.want)
			}
		})
	}
}

func TestHarnessCompatWorkflowSafe(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), ".github", "workflows", "harness-compat.yml"))
	if err != nil {
		t.Fatal(err)
	}
	w := string(b)
	for _, bad := range []string{"pull_request", "secrets.", "environment:", "cache: true", "actions/cache"} {
		if strings.Contains(w, bad) {
			t.Errorf("slice 1 workflow must not contain %q", bad)
		}
	}
	for _, want := range []string{"workflow_dispatch:", "permissions:\n  contents: read", "persist-credentials: false", "cache: false", "concurrency:"} {
		if !strings.Contains(w, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
	if strings.Contains(w, "schedule:") {
		t.Error("slice 1 is dispatch-only")
	}
}
