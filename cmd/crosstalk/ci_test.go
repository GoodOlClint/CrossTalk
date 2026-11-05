package main

import (
	"os"
	"path/filepath"
	"regexp"
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
