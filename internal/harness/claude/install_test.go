package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

type recorder struct {
	calls   []string
	plugins string
	fail    string
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if r.fail == r.calls[len(r.calls)-1] {
		return nil, errors.New("forced failure")
	}
	if strings.Join(args, " ") == "plugin list --json" {
		if r.plugins == "" {
			return []byte("[]"), nil
		}
		return []byte(r.plugins), nil
	}
	return nil, nil
}

func (r *recorder) index(prefix string) int {
	return slices.IndexFunc(r.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func rules(m map[string]any, kind string) []string {
	p, _ := m["permissions"].(map[string]any)
	var out []string
	for _, r := range p[kind].([]any) {
		out = append(out, r.(string))
	}
	return out
}

func installEnv(t *testing.T, r *recorder) harness.InstallEnv {
	t.Helper()
	home := t.TempDir()
	old := managedSettingsPath
	managedSettingsPath = filepath.Join(t.TempDir(), "managed-settings.json")
	t.Cleanup(func() { managedSettingsPath = old })
	return harness.InstallEnv{Bin: "/opt/x/crosstalk", Version: "9.9.9", Home: home, Run: r.run, Marketplace: "GoodOlClint/CrossTalk"}
}

// applyAll applies steps in order and stops at the first error, as install does.
func applyAll(steps []harness.Step) error {
	for _, s := range steps {
		if err := s.Apply(context.Background()); err != nil {
			return err
		}
	}
	return nil
}

func TestClaudeInstallSteps(t *testing.T) {
	r := &recorder{}
	env := installEnv(t, r)
	writeFile(t, filepath.Join(env.Home, ".claude.json"), `{"mcpServers": {"crosstalk": {"type": "stdio", "command": "crosstalk", "args": ["mcp"], "env": {"CROSSTALK_CODEX_ENDPOINTS": "unix:///s.sock", "A": "1"}}}}`)
	h := &Harness{}
	if err := applyAll(h.InstallSteps(env)); err != nil {
		t.Fatal(err)
	}
	add := "claude mcp add --scope user crosstalk -e A=1 -e CROSSTALK_CODEX_ENDPOINTS=unix:///s.sock -- /opt/x/crosstalk mcp"
	pi, rm, ad := r.index("claude plugin install crosstalk@crosstalk"), r.index("claude mcp remove --scope user crosstalk"), r.index(add)
	if pi < 0 || rm < 0 || ad < 0 || !(pi < rm && rm < ad) {
		t.Errorf("want plugin install, then mcp remove, then %q; got\n%s", add, strings.Join(r.calls, "\n"))
	}

	r2 := &recorder{plugins: `[{"id": "crosstalk@crosstalk", "version": "0.1.2"}]`}
	env.Run = r2.run
	if err := applyAll(h.InstallSteps(env)); err != nil {
		t.Fatal(err)
	}
	if r2.index("claude plugin install") >= 0 || r2.index("claude plugin update crosstalk@crosstalk") < 0 || r2.index("claude plugin marketplace update crosstalk") < 0 {
		t.Errorf("installed plugin not updated:\n%s", strings.Join(r2.calls, "\n"))
	}
}

func TestClaudeAllowRulePort(t *testing.T) {
	r := &recorder{}
	env := installEnv(t, r)
	user := filepath.Join(env.Home, ".claude", "settings.json")
	orig := `{"model": "x", "permissions": {"deny": ["mcp__plugin_crosstalk_crosstalk__ask", "Bash(rm:*)"], "allow": ["mcp__plugin_crosstalk_crosstalk"]}}`
	writeFile(t, user, orig)
	if err := applyAll((&Harness{}).InstallSteps(env)); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, user)
	if m["model"] != "x" {
		t.Errorf("unrelated key lost: %v", m)
	}
	if d := rules(m, "deny"); !slices.Equal(d, []string{"mcp__crosstalk__ask", "Bash(rm:*)"}) {
		t.Errorf("deny %v", d)
	}
	if a := rules(m, "allow"); !slices.Equal(a, []string{"mcp__crosstalk", "mcp__crosstalk__*"}) {
		t.Errorf("allow %v", a)
	}
	if b, err := os.ReadFile(user + ".crosstalk-backup"); err != nil || string(b) != orig {
		t.Errorf("backup %q %v", b, err)
	}
}

func TestClaudeRulePortStopsOnProjectDeny(t *testing.T) {
	r := &recorder{}
	env := installEnv(t, r)
	proj := filepath.Join(env.Home, "src", "proj")
	cj, _ := json.Marshal(map[string]any{"projects": map[string]any{proj: map[string]any{}}})
	writeFile(t, filepath.Join(env.Home, ".claude.json"), string(cj))
	shared := filepath.Join(proj, ".claude", "settings.json")
	local := filepath.Join(proj, ".claude", "settings.local.json")
	writeFile(t, shared, `{"permissions": {"deny": ["mcp__plugin_crosstalk_crosstalk__send_message"]}}`)
	writeFile(t, local, `{"permissions": {"deny": ["mcp__plugin_crosstalk_crosstalk__send_message"]}}`)
	user := filepath.Join(env.Home, ".claude", "settings.json")
	writeFile(t, user, `{}`)

	err := applyAll((&Harness{}).InstallSteps(env))
	if err == nil || !strings.Contains(err.Error(), shared) || !strings.Contains(err.Error(), "mcp__plugin_crosstalk_crosstalk__send_message") {
		t.Fatalf("want an error naming %s and the rule, got %v", shared, err)
	}
	if r.index("claude mcp add") >= 0 {
		t.Errorf("mcp add ran: %v", r.calls)
	}
	if b, _ := os.ReadFile(user); string(b) != "{}" {
		t.Errorf("user settings changed: %s", b)
	}
	if d := rules(readSettings(t, local), "deny"); !slices.Equal(d, []string{"mcp__crosstalk__send_message"}) {
		t.Errorf("local deny %v", d)
	}
	if b, _ := os.ReadFile(shared); !strings.Contains(string(b), "mcp__plugin_crosstalk_crosstalk__send_message") {
		t.Errorf("checked-in file edited: %s", b)
	}

	env.SkipRulePort = true
	if err := applyAll((&Harness{}).InstallSteps(env)); err != nil {
		t.Fatalf("--skip-rule-port: %v", err)
	}
	if r.index("claude mcp add") < 0 {
		t.Error("--skip-rule-port did not register the server")
	}
}

func TestClaudeMCPReplaceRestoresOnFailure(t *testing.T) {
	r := &recorder{fail: "claude mcp add --scope user crosstalk -e A=1 -- /opt/x/crosstalk mcp"}
	env := installEnv(t, r)
	writeFile(t, filepath.Join(env.Home, ".claude.json"), `{"mcpServers": {"crosstalk": {"command": "crosstalk", "args": ["mcp"], "env": {"A": "1"}}}}`)
	if err := mcpStep(env).Apply(context.Background()); err == nil {
		t.Fatal("want the add failure")
	}
	restore := "claude mcp add --scope user crosstalk -e A=1 -- crosstalk mcp"
	if i, j := r.index(r.fail), r.index(restore); i < 0 || j < i {
		t.Errorf("old entry not restored after the failed add:\n%s", strings.Join(r.calls, "\n"))
	}

	r2 := &recorder{}
	env.Run = r2.run
	writeFile(t, filepath.Join(env.Home, ".claude.json"), `{}`)
	writeFile(t, recoveryPath(env.Home), `{"command": "crosstalk", "args": ["mcp"], "env": {"B": "2"}}`)
	if err := mcpStep(env).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r2.index("claude mcp add --scope user crosstalk -e B=2 -- /opt/x/crosstalk mcp") < 0 {
		t.Errorf("env from the interrupted run not carried:\n%s", strings.Join(r2.calls, "\n"))
	}
	if _, err := os.Stat(recoveryPath(env.Home)); !os.IsNotExist(err) {
		t.Errorf("recovery copy left after a successful add: %v", err)
	}
}
