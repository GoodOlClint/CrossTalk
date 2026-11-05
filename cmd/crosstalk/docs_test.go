package main

import (
	"strings"
	"testing"
)

func TestSetupDocCoversGaps(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"brew shellenv", "@unverified", "bubblewrap", "folder-trust",
		"install.sh", "marketplace remove", "codex-path",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
	if strings.Contains(doc, "not yet on a clean account") {
		t.Error("setup.md still says install has not run on a clean account")
	}
}
