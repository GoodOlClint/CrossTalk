package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

const pluginDir = "../../plugins/crosstalk"

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func TestEvalFixtureNoticesMatchConst(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(pluginDir, "evals", "*", "prompt.md"))
	re := regexp.MustCompile(`notice="([^"]*)"`)
	seen := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen++
			if m[1] != claude.Notice {
				t.Errorf("%s: notice differs from claude.Notice: %q", f, m[1])
			}
		}
	}
	if seen < 5 {
		t.Errorf("found %d fixture notices, want at least 5", seen)
	}
}

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"version"}, &out, &out); code != 0 || strings.TrimSpace(out.String()) != mcpserver.Version {
		t.Fatalf("code %d out %q", code, out.String())
	}
}

func TestManifestsMatchBinary(t *testing.T) {
	for _, f := range []string{".claude-plugin/plugin.json", ".codex-plugin/plugin.json"} {
		m := readJSON(t, filepath.Join(pluginDir, f))
		if m["name"] != "crosstalk" || m["version"] != mcpserver.Version {
			t.Errorf("%s: name %v version %v, want crosstalk %s", f, m["name"], m["version"], mcpserver.Version)
		}
	}
	mk := readJSON(t, "../../.claude-plugin/marketplace.json")
	if mk["name"] != "crosstalk" {
		t.Errorf("claude marketplace name %v", mk["name"])
	}
	for _, p := range mk["plugins"].([]any) {
		if p.(map[string]any)["version"] != mcpserver.Version {
			t.Errorf("marketplace plugin version %v", p.(map[string]any)["version"])
		}
	}
	if readJSON(t, "../../.agents/plugins/marketplace.json")["name"] != "crosstalk" {
		t.Error("codex marketplace name")
	}
}

func TestServerKeysAndCommands(t *testing.T) {
	for f, approve := range map[string]bool{"claude.mcp.json": false, "codex.mcp.json": true} {
		srv := readJSON(t, filepath.Join(pluginDir, f))["mcpServers"].(map[string]any)
		if len(srv) != 1 {
			t.Fatalf("%s servers %v", f, srv)
		}
		s, ok := srv["crosstalk"].(map[string]any)
		if !ok {
			t.Fatalf("%s: server key is not exactly crosstalk: %v", f, srv)
		}
		args, _ := s["args"].([]any)
		if s["command"] != "crosstalk" || len(args) != 1 || args[0] != "mcp" {
			t.Errorf("%s command %v %v", f, s["command"], s["args"])
		}
		if got := s["default_tools_approval_mode"] == "approve"; got != approve {
			t.Errorf("%s approval mode %v", f, s["default_tools_approval_mode"])
		}
	}
}

var token = regexp.MustCompile("`([a-z]+(?:_[a-z]+)+)`")

func goSource(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir("../../internal", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		c, err := os.ReadFile(p)
		b.Write(c)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestSkillCoversStatesAndCodes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pluginDir, "skills/crosstalk/SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	skill := string(raw)
	for _, w := range []string{"accepted", "queued", "rejected", "unreachable", "unknown", "recovered", "bounced", "native_inform_only", "ambiguous_name", "ask_closed", "already_replied", "sender_verified"} {
		if !strings.Contains(skill, "`"+w+"`") {
			t.Errorf("skill omits %s", w)
		}
	}
	for r := range mcpserver.Hints {
		if !strings.Contains(skill, "`"+r+"`") {
			t.Errorf("skill omits hinted reason %s", r)
		}
	}
	src := goSource(t)
	for _, m := range token.FindAllStringSubmatch(skill, -1) {
		if !strings.Contains(src, `"`+m[1]) {
			t.Errorf("skill names %s, which the source does not define", m[1])
		}
	}
}

func TestMarketplacesPinToReleaseTag(t *testing.T) {
	tag := regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	for _, f := range []string{"../../.claude-plugin/marketplace.json", "../../.agents/plugins/marketplace.json"} {
		n := 0
		for _, p := range readJSON(t, f)["plugins"].([]any) {
			e := p.(map[string]any)
			if e["name"] != "crosstalk" {
				continue
			}
			n++
			src, _ := e["source"].(map[string]any)
			if len(src) != 4 || src["source"] != "git-subdir" || src["url"] != "https://github.com/GoodOlClint/CrossTalk.git" || src["path"] != "plugins/crosstalk" {
				t.Errorf("%s: source %v", f, e["source"])
				continue
			}
			if r, _ := src["ref"].(string); !tag.MatchString(r) {
				t.Errorf("%s: ref %q is not a release tag", f, src["ref"])
			}
		}
		if n != 1 {
			t.Errorf("%s: %d crosstalk entries", f, n)
		}
	}
}
