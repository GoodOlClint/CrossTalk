package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
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

func TestPluginShipsNoServer(t *testing.T) {
	for _, f := range []string{".claude-plugin/plugin.json", ".codex-plugin/plugin.json"} {
		if m := readJSON(t, filepath.Join(pluginDir, f)); m["mcpServers"] != nil {
			t.Errorf("%s declares mcpServers %v", f, m["mcpServers"])
		}
	}
	graders := 0
	err := filepath.WalkDir(pluginDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".mcp.json") {
			t.Errorf("plugin ships a server file: %s", p)
		}
		if filepath.Base(filepath.Dir(p)) != "graders" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "mcp__plugin_crosstalk") {
			t.Errorf("%s names the old tool prefix", p)
		}
		if strings.Contains(string(b), "mcp__crosstalk__") {
			graders++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if graders < 8 {
		t.Errorf("%d graders name mcp__crosstalk__, want at least 8", graders)
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

func TestVersionIs014AndManifestsMatch(t *testing.T) {
	if mcpserver.Version != "0.1.4" {
		t.Errorf("binary version %s, want 0.1.4", mcpserver.Version)
	}
	for _, f := range []string{filepath.Join(pluginDir, ".claude-plugin/plugin.json"), filepath.Join(pluginDir, ".codex-plugin/plugin.json")} {
		if v := readJSON(t, f)["version"]; v != mcpserver.Version {
			t.Errorf("%s version %v, binary %s", f, v, mcpserver.Version)
		}
	}
	for _, p := range readJSON(t, "../../.claude-plugin/marketplace.json")["plugins"].([]any) {
		if v := p.(map[string]any)["version"]; v != mcpserver.Version {
			t.Errorf("claude marketplace version %v, binary %s", v, mcpserver.Version)
		}
	}
}

func TestVersionStampedByLdflags(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "crosstalk")
	cmd := exec.Command("go", "build", "-o", bin, "-ldflags", "-X github.com/GoodOlClint/crosstalk/internal/mcpserver.Version=0.1.4-rc.1", ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "0.1.4-rc.1" {
		t.Errorf("version = %q, %v; want 0.1.4-rc.1", out, err)
	}
}
