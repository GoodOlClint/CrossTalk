package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	mdLinkRE  = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	codeFence = regexp.MustCompile("(?s)```.*?```")
)

func unpublished(rel string) bool {
	return strings.HasPrefix(rel, "plans/") || strings.HasPrefix(rel, "spikes/")
}

func TestSiteConfig(t *testing.T) {
	cfg := repoFile(t, "docs/_config.yml")
	if !regexp.MustCompile(`(?m)^url:\s*"?https://getcrosstalk\.dev"?\s*$`).MatchString(cfg) {
		t.Error("_config.yml: url is not https://getcrosstalk.dev")
	}
	for _, dir := range []string{"plans", "spikes"} {
		if !regexp.MustCompile(`(?m)^\s*-\s*"?` + dir + `/?"?\s*$`).MatchString(cfg) {
			t.Errorf("_config.yml: exclude lacks %s", dir)
		}
	}
	if !strings.Contains(cfg, "exclude:") {
		t.Error("_config.yml: no exclude list")
	}
	repoFile(t, "docs/index.md")
}

func TestSiteLinks(t *testing.T) {
	docs := filepath.Join(releaseRepoRoot(t), "docs")
	err := filepath.WalkDir(docs, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		rel, _ := filepath.Rel(docs, path)
		rel = filepath.ToSlash(rel)
		if unpublished(rel) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range mdLinkRE.FindAllStringSubmatch(codeFence.ReplaceAllString(string(b), ""), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			resolved, _ := filepath.Rel(docs, filepath.Join(filepath.Dir(path), target))
			resolved = filepath.ToSlash(resolved)
			switch {
			case strings.HasPrefix(resolved, ".."):
				t.Errorf("%s: link %s leaves the published tree", rel, m[1])
			case unpublished(resolved):
				t.Errorf("%s: link %s points into an unpublished directory", rel, m[1])
			default:
				if _, err := os.Stat(filepath.Join(docs, filepath.FromSlash(resolved))); err != nil {
					t.Errorf("%s: link %s does not exist", rel, m[1])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
