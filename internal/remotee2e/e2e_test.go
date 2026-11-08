package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func root(t *testing.T) string {
	t.Helper()
	r, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func scopeOf(t *testing.T, file string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	p := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "head")
	head := git("rev-parse", "HEAD")
	c := exec.Command(filepath.Join(root(t), "scripts", "release-build.sh"), "ci-scope", base, head)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("ci-scope: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestHarnessPathsAreCodeScope(t *testing.T) {
	for _, f := range []string{"scripts/remote-e2e/run.sh", "internal/remotee2e/main.go"} {
		if got := scopeOf(t, f); got != "code" {
			t.Errorf("%s scope = %q, want code", f, got)
		}
	}
	if got := scopeOf(t, "docs/contributing.md"); got != "docs" {
		t.Errorf("docs scope = %q, want docs", got)
	}
}

func TestRunShHasPairingFailureSwitch(t *testing.T) {
	run := read(t, "scripts/remote-e2e/run.sh")
	if !strings.Contains(run, "FAIL_PAIRING") {
		t.Error("run.sh has no FAIL_PAIRING switch")
	}
}

func TestCIJobGatedAndNotRequired(t *testing.T) {
	ci := read(t, ".github/workflows/ci.yml")
	i := strings.Index(ci, "\n  remote-e2e:")
	if i < 0 {
		t.Fatal("ci.yml has no remote-e2e job")
	}
	job := ci[i+1:]
	if loc := regexp.MustCompile(`\n  [a-z]`).FindStringIndex(job[2:]); loc != nil {
		job = job[:loc[0]+2]
	}
	for _, want := range []string{"release-build.sh ci-scope", "fetch-depth: 0", "timeout-minutes: 15", "if: always()", "FAIL_PAIRING"} {
		if !strings.Contains(job, want) {
			t.Errorf("remote-e2e job lacks %q", want)
		}
	}
	if n := strings.Count(job, "if: steps.scope.outputs.scope != 'docs'") + strings.Count(job, "&& steps.scope.outputs.scope != 'docs'"); n < 5 {
		t.Errorf("remote-e2e job guards %d steps against docs scope, want setup-go, build, scenario, broken-pairing and upload (5)", n)
	}
	if _, err := os.Stat(filepath.Join(root(t), ".github/workflows/spike-s6.yml")); err == nil {
		t.Error("spike-s6.yml still exists")
	}
	if _, err := os.Stat(filepath.Join(root(t), "docs/spikes/17-ci-two-host/run.sh")); err == nil {
		t.Error("spike code still under docs/spikes/17-ci-two-host/")
	}
}
