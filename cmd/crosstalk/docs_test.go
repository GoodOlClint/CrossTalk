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
		{"AGENTS refresh built", "AGENTS.md", "R4, F1 and F2", "Proposed, partly built"},
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

func TestDocsSetupNamesOpenCodeInstallChecks(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"`claude`, `codex`, `opencode` and `pi`", "crosstalk install opencode", "K13", "K2, K5 and K6",
		"Supported OpenCode versions", "2.4", "1.x", "ADR 0032", "Kilo",
		"`immediate`", "`next_turn`", "stored without a model turn", "paid model turn",
		"restart the OpenCode service", "refuses to rewrite a config that has comments",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
}

func TestDocsDesignAndAgentsNameOpenCode(t *testing.T) {
	design := repoFile(t, "docs/design.md")
	if strings.Contains(design, "Next harnesses (spike 11): OpenCode") {
		t.Error("design.md still lists OpenCode under next harnesses")
	}
	for _, want := range []string{
		"@opencode/cli", "2.4", "1.x", "ADR 0032", "ADR 0033",
		"not yet verified (slice 9)", "M3", "`next_turn`", "TestPooledSessionsNeverCrossBind",
		"TestInjectFailureStates", "TestInstallOpenCodeEndToEnd",
	} {
		if !strings.Contains(design, want) {
			t.Errorf("design.md lacks %q", want)
		}
	}
	rows := map[string]string{}
	for _, line := range strings.Split(design, "\n") {
		for _, n := range []string{"46", "47", "48", "49"} {
			if strings.HasPrefix(line, n+". ") {
				rows[n] = line
			}
		}
	}
	for n, wants := range map[string][]string{
		"46": {"not yet verified (slice 9)"},
		"47": {"not yet verified (slice 9)", "TestInjectFailureStates"},
		"49": {"Re-run 2026-11-24", "M3"},
	} {
		for _, want := range wants {
			if !strings.Contains(rows[n], want) {
				t.Errorf("design.md row %s lacks %q", n, want)
			}
		}
	}
	for _, want := range []string{"MailSignal", "fixed steer prompt", `"harness": "claude|codex|opencode|pi"`} {
		if !strings.Contains(design, want) {
			t.Errorf("design.md lacks %q", want)
		}
	}
	agents := repoFile(t, "AGENTS.md")
	if strings.Contains(agents, "OpenCode harness, planned") {
		t.Error("AGENTS.md still says OpenCode is planned and not built")
	}
	for _, want := range []string{"OpenCode harness, released in v0.3.0", "v0.3.0", "ADR 0032", "ADR 0033", "ADR 0034", "third harness"} {
		if !strings.Contains(agents, want) {
			t.Errorf("AGENTS.md lacks %q", want)
		}
	}
	for _, p := range []string{"docs/faq.md", "docs/how-it-works.md", "docs/examples.md", "docs/contributing.md"} {
		if !strings.Contains(repoFile(t, p), "OpenCode") {
			t.Errorf("%s does not name OpenCode", p)
		}
	}
}

func TestDocsSetupCoversPi(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"## Pi", "crosstalk install pi", "Supported Pi versions", "1.6.0", "ADR 0035", "ADR 0037",
		"extensions/crosstalk/index.ts", "$PI_CODING_AGENT_DIR", "pi mcp remove crosstalk", "writes no `mcp.json` entry",
		"`pi mcp list` does not show CrossTalk", "`/mcp` inside a Pi session", "`/reload` restarts a session's MCP servers",
		"wakes the session with a fixed line", "`receive_message`", "paid model turn",
		"no approval prompt", "K3 and K4 do not apply", "K14", "K1, K3, K4, K9, K10 and K13 do not apply to Pi",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
}

func TestDocsAndPluginNamePi(t *testing.T) {
	for p, wants := range map[string][]string{
		"docs/design.md": {
			`"harness": "claude|codex|opencode|pi"`, "1.6.0", "ADR 0035", "ADR 0036", "ADR 0037",
			"TestInjectStateTable", "TestInstallPiEndToEnd", "not yet verified (slice 9)",
		},
		"README.md":                       {"Claude Code, Codex, OpenCode and Pi", "not yet in a release"},
		"docs/setup.md":                   {"restart those: end and restart Claude and Codex sessions, restart the OpenCode service, and run `/reload` in each Pi session", "OpenCode loads its MCP entries when its service starts, so restart"},
		"docs/faq.md":                     {"Pi", "1.6.0", "crosstalk install pi", "not been run live"},
		"docs/how-it-works.md":            {"Pi", "1.6.0", "crosstalk install pi", "no approval prompt"},
		"docs/examples.md":                {"OpenCode", "Pi"},
		"docs/contributing.md":            {"internal/harness/pi"},
		"docs/index.md":                   {"OpenCode", "Pi", "crosstalk install pi"},
		"docs/why.md":                     {"OpenCode or Pi"},
		"docs/_config.yml":                {"Claude Code, Codex, OpenCode and Pi"},
		"AGENTS.md":                       {"pi-linux", "pi-macos", "@earendil-works/pi-coding-agent", "slices 1 to 7"},
		"docs/plans/harness-compat-ci.md": {"pi-linux", "pi-macos", "@earendil-works/pi-coding-agent"},
		"plugins/crosstalk/skills/crosstalk/SKILL.md":  {"(Claude Code, Codex, OpenCode, Pi)", "Claude, Codex, OpenCode or Pi"},
		"plugins/crosstalk/README.md":                  {"Claude Code, Codex, OpenCode and Pi"},
		"plugins/crosstalk/.claude-plugin/plugin.json": {"(Claude Code, Codex, OpenCode, Pi)"},
		"plugins/crosstalk/.codex-plugin/plugin.json":  {"(Claude Code, Codex, OpenCode, Pi)"},
		".claude-plugin/marketplace.json":              {"(Claude Code, Codex, OpenCode, Pi)"},
	} {
		doc := repoFile(t, p)
		for _, want := range wants {
			if !strings.Contains(doc, want) {
				t.Errorf("%s lacks %q", p, want)
			}
		}
		if strings.Contains(doc, "(Claude Code, Codex)") {
			t.Errorf("%s still names two harnesses", p)
		}
	}
}
