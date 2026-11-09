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

func TestSetupDocCoversGapsRemote(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"arrives in 0.2.0 and is not yet released", "has not been run live", "crosstalk remote unpair",
		"crosstalk remote config", "crosstalk remote serve", "crosstalk remote pair", "crosstalk remote join",
		"crosstalk remote status", "crosstalk remote status --json", "crosstalk remote enable", "crosstalk remote disable",
		"crosstalk install --remote", "crosstalk reinstall --remote",
		"--bind", "--port", "--alias", "--force", "--yes", "--json", "7745", "name@alias",
		"next: crosstalk remote pair on this host, crosstalk remote join on the other",
		"Set up remote messaging to your other machines? This starts a network listener at login. [y/N]",
		"~/Library/LaunchAgents/dev.getcrosstalk.remote.plist", "~/.config/systemd/user/crosstalk-remote.service", "~/Library/Logs/crosstalk-remote.log",
		"service running, port not bound", "unit_sha256", "a gateway is running (pid N)", "loginctl enable-linger", "unpair this host on:",
		"Local Network",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
	if strings.Contains(doc, "draft, not in 0.1.x") {
		t.Error("setup.md still calls remote a draft not in 0.1.x")
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

func TestSetupDocCoversPairing(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"crosstalk remote pair", "crosstalk remote join", "crosstalk-pair:v1:",
		"Paste the token from the other host:", "stdin only", "expires after 10 minutes", "one pairing attempt per second",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
}

func TestSetupDocCoversRemoteFirewallApprovalRefusal(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"inbound access on its port (default 7745) and outbound access to paired hosts",
		"no per-firewall steps are given",
		"has not approved CrossTalk tool calls lets an `ask` expire",
		"`remote enable` refuses a binary older than the running one",
		"crosstalk reinstall",
		"brew upgrade crosstalk",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
}
