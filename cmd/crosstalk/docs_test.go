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
		"brew install goodolclint/tap/crosstalk", "brew uninstall crosstalk",
		"crosstalk uninstall` before `brew uninstall crosstalk", "even when only a local- or project-scope entry exists", "$(brew --prefix)/bin/crosstalk",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
	if strings.Contains(doc, "not yet on a clean account") {
		t.Error("setup.md still says install has not run on a clean account")
	}
}

func TestUpgradeFromBundledServerDocumented(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"Upgrading from a plugin that bundled the server (before 0.1.3)",
		"brew install goodolclint/tap/crosstalk",
		"crosstalk install --yes", "crosstalk install --check", "codex mcp list",
		"mcp__plugin_crosstalk_crosstalk__", "scratch marketplace",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
	skill := repoFile(t, "plugins/crosstalk/skills/crosstalk/SKILL.md")
	for _, want := range []string{"crosstalk install", "brew install goodolclint/tap/crosstalk", "restart"} {
		if !strings.Contains(skill, want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
}
