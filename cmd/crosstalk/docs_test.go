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
		"arrives in 0.2.0", "has not been run over a tailnet", "crosstalk remote unpair",
		"crosstalk remote config", "crosstalk remote serve", "crosstalk remote pair", "crosstalk remote join",
		"crosstalk remote status", "crosstalk remote status --json", "crosstalk remote enable", "crosstalk remote disable",
		"crosstalk install --remote", "crosstalk reinstall --remote",
		"--bind", "--port", "--alias", "--force", "--yes", "--json", "7745", "name@alias",
		"next: crosstalk remote pair on this host, crosstalk remote join on the other",
		"Set up remote messaging to your other machines? This starts a network listener at login. [y/N]",
		"~/Library/LaunchAgents/dev.getcrosstalk.remote.plist", "~/.config/systemd/user/crosstalk-remote.service", "~/Library/Logs/crosstalk-remote.log",
		"service running, port not bound", "unit_sha256", "a gateway is running (pid N)", "loginctl enable-linger", "unpair this host on:",
		"Local Network",
		"### Choose an address", "### When a host changes networks",
		"--bind all needs --advertise, the address or name peers dial", "crosstalk remote config --bind all --advertise",
		"service running, waiting for address", "peers still dial the old address: pair again with",
		"fingerprint pin is the only trust", "### Restart after changing the bind", "--bind 100.64.1.2 --advertise host.example.ts.net", "Avahi", "MagicDNS", "100.64.0.0/10", "never waits for an address",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
	for _, stale := range []string{"Each host binds one fixed IP address", "If a host's address changes, pair again."} {
		if strings.Contains(doc, stale) {
			t.Errorf("setup.md still says %q", stale)
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

func TestSetupDocCoversStaleShims(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"K11", "keep the old binary until", "codex app-server daemon restart",
		"config/mcpServer/reload", "service running, handshake failed", "service running, certificate mismatch",
		"launchctl kickstart -k gui/<uid>/dev.getcrosstalk.remote", "systemctl --user restart crosstalk-remote.service",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
}

// A refresh does not wait for an A2A client blocked in await (#239 item 4);
// ADR 0030 and the setup doc name the limit.
func TestRefreshAwaitLimitDocumented(t *testing.T) {
	for _, f := range []string{"docs/decisions/0030-the-shim-re-execs-itself-in-place-when-its-binary-is-replaced.md", "docs/setup.md"} {
		doc := repoFile(t, f)
		for _, want := range []string{"blocked in a default-wait", "SendMessage", "drops", "`unknown`"} {
			if !strings.Contains(doc, want) {
				t.Errorf("%s lacks %q", f, want)
			}
		}
	}
}

func TestSetupDocCoversAutoRefresh(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"CROSSTALK_NO_AUTO_REFRESH", "--no-auto-refresh",
		"upgrade/<version>/<target>.done", "follow-up.log", "plugin step", "gateway restart", "without a terminal",
		"refresh on their own", "manual path", "have not refreshed",
		"keeps its original tool list", "follow-up is off too", "already wrote the markers",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
	for _, old := range []string{
		"Running sessions and the remote gateway keep the old binary until restarted",
		"A running session keeps the old server",
		"then restart sessions: running sessions, and the remote gateway, keep the old binary until they are restarted",
	} {
		if strings.Contains(doc, old) {
			t.Errorf("setup.md still says %q", old)
		}
	}
}

func TestUpgradeRefreshADREdits(t *testing.T) {
	const (
		a0006 = "docs/decisions/0006-no-spawning-the-mcp-server-is-the-shim-and-binds-from-harness-provided-identity.md"
		a0016 = "docs/decisions/0016-crosstalk-install-registers-mcp-at-the-installed-path.md"
		a0018 = "docs/decisions/0018-homebrew-tap-formula-and-crosstalk-install-registers-the-homebrew-path.md"
		a0028 = "docs/decisions/0028-codex-no-daemon-sessions-get-an-experimental-opt-in-queue-wake-the-journal-stays-the-mailbox.md"
	)
	for _, c := range []struct {
		name, file, has, lacks string
	}{
		{"0006 names the follow-up exception", a0006, "ADR 0031", ""},
		{"0006 probe", a0006, "compatibility probe", ""},
		{"0016 unconfirmed steps", a0016, "run unconfirmed", ""},
		{"0016 sessions refresh", a0016, "refresh on their own", "running sessions keep their old server until restarted"},
		{"0018 sandbox evidence", a0018, "spike 20", ""},
		{"0018 plugin consequence", a0018, "ADR 0031", "the plugin stays at the old version until the user runs `crosstalk install` again (caveats say so; K1 reports it)"},
		{"0028 item 4 quiescence", a0028, "open wake transaction", ""},
		{"0028 item 8 handoff", a0028, "A refresh (ADR 0030) carries each proven claim", ""},
		{"AGENTS names 0030", "AGENTS.md", "ADR 0030", ""},
		{"AGENTS names 0031", "AGENTS.md", "ADR 0031", "follow the step 5 merge"},
		{"AGENTS refresh built", "AGENTS.md", "R4 and F2", "Proposed, partly built"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := repoFile(t, c.file)
			if !strings.Contains(doc, c.has) {
				t.Errorf("%s lacks %q", c.file, c.has)
			}
			if c.lacks != "" && strings.Contains(doc, c.lacks) {
				t.Errorf("%s still says %q", c.file, c.lacks)
			}
		})
	}
}

func TestFAQCoversAutoRefresh(t *testing.T) {
	doc := repoFile(t, "docs/faq.md")
	for _, want := range []string{"refresh on their own", "CROSSTALK_NO_AUTO_REFRESH=1", "started before the feature existed"} {
		if !strings.Contains(doc, want) {
			t.Errorf("faq.md lacks %q", want)
		}
	}
}
