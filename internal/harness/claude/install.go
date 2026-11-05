package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

var _ harness.Installer = (*Harness)(nil)

const (
	// oldPrefix names the tools of the plugin-supplied server (0.1.2 and
	// earlier); newPrefix the user-scope server crosstalk install registers.
	oldPrefix = "mcp__plugin_crosstalk_crosstalk"
	newPrefix = "mcp__crosstalk"
	AllowRule = newPrefix + "__*"
	pluginID  = "crosstalk@crosstalk"
)

var managedSettingsPath = map[string]string{
	"darwin": "/Library/Application Support/ClaudeCode/managed-settings.json",
	"linux":  "/etc/claude-code/managed-settings.json",
}[runtime.GOOS]

var ruleKinds = []string{"allow", "ask", "deny"}

func (h *Harness) Found() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

func userSettingsPath(home string) string { return filepath.Join(home, ".claude", "settings.json") }
func claudeJSONPath(home string) string   { return filepath.Join(home, ".claude.json") }

func portRule(r string) (string, bool) {
	rest, ok := strings.CutPrefix(r, oldPrefix)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "_") && !strings.HasPrefix(rest, "(")) {
		return r, false
	}
	return newPrefix + rest, true
}

// readJSONFile returns the parsed object and its bytes; a missing file is
// an empty object and nil bytes.
func readJSONFile(path string) (map[string]any, []byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	m := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return m, b, nil
}

func writeJSONFile(path string, m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return harness.ReplaceFile(path, append(b, '\n'))
}

func ruleList(m map[string]any, kind string) []any {
	p, _ := m["permissions"].(map[string]any)
	l, _ := p[kind].([]any)
	return l
}

func setRuleList(m map[string]any, kind string, l []any) {
	p, _ := m["permissions"].(map[string]any)
	if p == nil {
		p = map[string]any{}
		m["permissions"] = p
	}
	if len(l) == 0 {
		delete(p, kind)
		if len(p) == 0 {
			delete(m, "permissions")
		}
		return
	}
	p[kind] = l
}

type ruleSource struct {
	path     string
	editable bool
}

// ruleSources lists every permission file install can find: the user file,
// the managed file, and the checked-in and local files of each project
// listed in ~/.claude.json.
func ruleSources(home string) ([]ruleSource, error) {
	srcs := []ruleSource{{userSettingsPath(home), true}}
	if managedSettingsPath != "" {
		srcs = append(srcs, ruleSource{managedSettingsPath, false})
	}
	cj, _, err := readJSONFile(claudeJSONPath(home))
	if err != nil {
		return nil, err
	}
	projects, _ := cj["projects"].(map[string]any)
	dirs := make([]string, 0, len(projects))
	for d := range projects {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		srcs = append(srcs,
			ruleSource{filepath.Join(d, ".claude", "settings.json"), false},
			ruleSource{filepath.Join(d, ".claude", "settings.local.json"), true})
	}
	return srcs, nil
}

type oldRule struct {
	path, kind, rule string
	editable         bool
}

// oldRules finds every rule naming the old prefix. Unreadable files are
// skipped: a listed project directory may be gone.
func oldRules(home string) ([]oldRule, error) {
	srcs, err := ruleSources(home)
	if err != nil {
		return nil, err
	}
	var out []oldRule
	for _, s := range srcs {
		m, _, err := readJSONFile(s.path)
		if err != nil {
			continue
		}
		for _, k := range ruleKinds {
			for _, r := range ruleList(m, k) {
				if rs, ok := r.(string); ok {
					if _, old := portRule(rs); old {
						out = append(out, oldRule{s.path, k, rs, s.editable})
					}
				}
			}
		}
	}
	return out, nil
}

// blocking reports the old-prefix rules install cannot port: a deny or ask
// in a checked-in or managed file stops matching after the rename while the
// new allow grants the tool.
func blocking(rs []oldRule) []oldRule {
	var out []oldRule
	for _, r := range rs {
		if !r.editable && r.kind != "allow" {
			out = append(out, r)
		}
	}
	return out
}

func describe(rs []oldRule) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "%s: %s %s\n", r.path, r.kind, r.rule)
	}
	return b.String()
}

func portFile(path string) error {
	m, _, err := readJSONFile(path)
	if err != nil {
		return err
	}
	changed := false
	for _, k := range ruleKinds {
		l := ruleList(m, k)
		for i, r := range l {
			if rs, ok := r.(string); ok {
				if n, old := portRule(rs); old {
					l[i] = n
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil
	}
	return writeJSONFile(path, m)
}

type pluginEntry struct {
	ID         string         `json:"id"`
	Version    string         `json:"version"`
	MCPServers map[string]any `json:"mcpServers"`
}

func installedPlugin(ctx context.Context, env harness.InstallEnv) (*pluginEntry, error) {
	out, err := env.Run(ctx, "claude", "plugin", "list", "--json")
	if err != nil {
		return nil, err
	}
	var ps []pluginEntry
	if err := json.Unmarshal(out, &ps); err != nil {
		return nil, fmt.Errorf("claude plugin list --json: %w", err)
	}
	for _, p := range ps {
		if p.ID == pluginID {
			return &p, nil
		}
	}
	return nil, nil
}

func pluginStep(env harness.InstallEnv) harness.Step {
	return harness.Step{
		Title:  "Claude plugin",
		Change: "claude plugin marketplace add GoodOlClint/CrossTalk, then claude plugin install " + pluginID + " (marketplace update and plugin update when it is installed)",
		Apply: func(ctx context.Context) error {
			if _, err := env.Run(ctx, "claude", "plugin", "marketplace", "add", "GoodOlClint/CrossTalk"); err != nil {
				return err
			}
			p, err := installedPlugin(ctx, env)
			if err != nil {
				return err
			}
			if p == nil {
				_, err = env.Run(ctx, "claude", "plugin", "install", pluginID)
				return err
			}
			if _, err := env.Run(ctx, "claude", "plugin", "marketplace", "update", "crosstalk"); err != nil {
				return err
			}
			_, err = env.Run(ctx, "claude", "plugin", "update", pluginID)
			return err
		},
	}
}

func rulePortStep(env harness.InstallEnv) (harness.Step, bool) {
	rs, err := oldRules(env.Home)
	if err != nil {
		return harness.Step{Title: "permission rules", Apply: func(context.Context) error { return err }}, true
	}
	if len(rs) == 0 {
		return harness.Step{}, false
	}
	var edit []oldRule
	for _, r := range rs {
		if r.editable {
			edit = append(edit, r)
		}
	}
	block := blocking(rs)
	change := ""
	if len(edit) > 0 {
		change += "rewrite to " + newPrefix + "__ (backup next to each file):\n" + describe(edit)
	}
	if len(block) > 0 {
		change += "not edited; port these by hand before install continues, or pass --skip-rule-port:\n" + describe(block)
	}
	return harness.Step{
		Title:  "permission rules on the old tool prefix",
		Change: change,
		Apply: func(context.Context) error {
			for _, p := range uniquePaths(edit) {
				if err := portFile(p); err != nil {
					return err
				}
			}
			if len(block) > 0 && !env.SkipRulePort {
				return fmt.Errorf("old-prefix rules install does not edit; port them to %s__ by hand or pass --skip-rule-port:\n%s", newPrefix, describe(block))
			}
			return nil
		},
	}, true
}

func uniquePaths(rs []oldRule) []string {
	var out []string
	for _, r := range rs {
		if !slices.Contains(out, r.path) {
			out = append(out, r.path)
		}
	}
	return out
}

type mcpEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// userEntry reads the user-scope crosstalk server from ~/.claude.json, where
// claude mcp add --scope user writes it.
func userEntry(home string) (*mcpEntry, error) {
	b, err := os.ReadFile(claudeJSONPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cj struct {
		MCPServers map[string]*mcpEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cj); err != nil {
		return nil, fmt.Errorf("%s: %w", claudeJSONPath(home), err)
	}
	return cj.MCPServers["crosstalk"], nil
}

func mcpStep(env harness.InstallEnv) harness.Step {
	change := "claude mcp add --scope user crosstalk -- " + env.Bin + " mcp"
	if e, _ := userEntry(env.Home); e != nil {
		change += fmt.Sprintf("\nreplaces the user entry %q %q, keeping its env %v", e.Command, e.Args, sortedKeys(e.Env))
	}
	return harness.Step{
		Title:  "Claude MCP server",
		Change: change,
		Apply: func(ctx context.Context) error {
			e, err := userEntry(env.Home)
			if err != nil {
				return err
			}
			args := []string{"mcp", "add", "--scope", "user", "crosstalk"}
			if e != nil {
				if _, err := env.Run(ctx, "claude", "mcp", "remove", "--scope", "user", "crosstalk"); err != nil {
					return err
				}
				for _, k := range sortedKeys(e.Env) {
					args = append(args, "-e", k+"="+e.Env[k])
				}
			}
			_, err = env.Run(ctx, "claude", append(args, "--", env.Bin, "mcp")...)
			return err
		},
	}
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func allowStep(env harness.InstallEnv) harness.Step {
	path := userSettingsPath(env.Home)
	return harness.Step{
		Title:  "Claude allow rule",
		Change: "add " + AllowRule + " to permissions.allow in " + path,
		Apply: func(context.Context) error {
			m, _, err := readJSONFile(path)
			if err != nil {
				return err
			}
			l := ruleList(m, "allow")
			if slices.Contains(l, any(AllowRule)) {
				return nil
			}
			setRuleList(m, "allow", append(l, AllowRule))
			return writeJSONFile(path, m)
		},
	}
}

// InstallSteps ports old-prefix rules before the server is registered, so a
// rule port that stops leaves no new server and no new allow behind.
func (h *Harness) InstallSteps(env harness.InstallEnv) []harness.Step {
	steps := []harness.Step{pluginStep(env)}
	if s, ok := rulePortStep(env); ok {
		steps = append(steps, s)
	}
	return append(steps, mcpStep(env), allowStep(env))
}

func (h *Harness) UninstallSteps(env harness.InstallEnv) []harness.Step { return nil }

func (h *Harness) Checks(ctx context.Context, env harness.InstallEnv) []harness.CheckResult {
	return nil
}
