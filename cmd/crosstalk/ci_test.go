package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDependabotEcosystems(t *testing.T) {
	cfg := repoFile(t, ".github/dependabot.yml")
	for _, eco := range []string{"gomod", "github-actions"} {
		re := regexp.MustCompile(`(?m)^\s*- package-ecosystem: "?` + eco + `"?\s*\n\s*directory: "/"`)
		if !re.MatchString(cfg) {
			t.Errorf("dependabot.yml lacks ecosystem %s with directory \"/\"", eco)
		}
	}
	if n := strings.Count(cfg, "interval: weekly"); n != 2 {
		t.Errorf("want 2 weekly schedules, got %d", n)
	}
	if n := strings.Count(cfg, "groups:"); n != 2 {
		t.Errorf("want one grouped PR per ecosystem (2 groups blocks), got %d", n)
	}
}

var actionFloor = map[string]int{
	"checkout":                6,
	"cache":                   5,
	"upload-artifact":         5,
	"download-artifact":       6,
	"create-github-app-token": 3,
}

var (
	usesRE     = regexp.MustCompile(`(?m)^\s*(?:- )?uses:\s*(\S+)`)
	stepSplit  = regexp.MustCompile(`\n\s*- `)
	sha40RE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	topPermsRE = regexp.MustCompile(`(?m)^permissions:`)
	jobNameRE  = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)
)

func workflowFiles(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(releaseRepoRoot(t), ".github", "workflows", "*.yml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	out := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = string(b)
	}
	return out
}

func workflowJobs(src string) map[string]string {
	jobs := map[string]string{}
	var name string
	inJobs := false
	for _, line := range strings.Split(src, "\n") {
		switch {
		case line == "jobs:":
			inJobs = true
		case inJobs && len(line) > 0 && line[0] != ' ' && line[0] != '#':
			inJobs = false
			name = ""
		case inJobs:
			if m := jobNameRE.FindStringSubmatch(line); m != nil {
				name = m[1]
			}
		}
		if inJobs && name != "" {
			jobs[name] += line + "\n"
		}
	}
	return jobs
}

func TestWorkflowHygiene(t *testing.T) {
	for file, src := range workflowFiles(t) {
		if !topPermsRE.MatchString(src) {
			t.Errorf("%s: no top-level permissions:", file)
		}
		for _, m := range usesRE.FindAllStringSubmatchIndex(src, -1) {
			ref := src[m[2]:m[3]]
			if strings.HasPrefix(ref, "./") {
				continue
			}
			action, ver, _ := strings.Cut(ref, "@")
			owner, name, _ := strings.Cut(action, "/")
			if owner != "actions" {
				if !sha40RE.MatchString(ver) {
					t.Errorf("%s: %s is not pinned to a 40-character SHA", file, ref)
				}
				continue
			}
			if floor, ok := actionFloor[name]; ok {
				n, err := strconv.Atoi(strings.TrimPrefix(ver, "v"))
				if err != nil || n < floor {
					t.Errorf("%s: %s is below the floor v%d", file, ref, floor)
				}
			}
			if name == "checkout" {
				rest := src[m[1]:]
				if loc := stepSplit.FindStringIndex(rest); loc != nil {
					rest = rest[:loc[0]]
				}
				if !strings.Contains(rest, "persist-credentials: false") {
					t.Errorf("%s: checkout step lacks persist-credentials: false", file)
				}
			}
		}
	}
	for job, body := range workflowJobs(workflowFiles(t)["release.yml"]) {
		if !strings.Contains(body, "\n    permissions:") {
			t.Errorf("release.yml: job %s declares no permissions:", job)
		}
	}
	ci := workflowFiles(t)["ci.yml"]
	if !strings.Contains(ci, "\nconcurrency:") || !strings.Contains(ci, "cancel-in-progress: ${{ github.event_name == 'pull_request' }}") {
		t.Error("ci.yml: no top-level concurrency group cancelling superseded PR runs")
	}
	vuln := workflowJobs(ci)["govulncheck"]
	if !regexp.MustCompile(`go run golang\.org/x/vuln/cmd/govulncheck@v\d+\.\d+\.\d+ \./\.\.\.`).MatchString(vuln) {
		t.Error("ci.yml: no govulncheck job running a version-pinned govulncheck")
	}
	for _, trigger := range []string{"\n  pull_request:", "\n  push:", "\n  schedule:"} {
		if !strings.Contains(ci, trigger) {
			t.Errorf("ci.yml: trigger %q missing", strings.TrimSpace(trigger))
		}
	}
}
