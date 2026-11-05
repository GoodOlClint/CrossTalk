package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

var _ harness.Installer = (*Harness)(nil)

// serverTable must stay "crosstalk": appserver/methods.go delivers into the
// namespace of that name.
const (
	serverTable = "mcp_servers.crosstalk"
	pluginID    = "crosstalk@crosstalk"
)

func (h *Harness) Found() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

func codexHome(home string) string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".codex")
}

func configPath(home string) string { return filepath.Join(codexHome(home), "config.toml") }

var (
	headerRe = regexp.MustCompile(`^\s*\[\[?([^\[\]]+)\]\]?\s*(#.*)?$`)
	keyRe    = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=\s*(.*)$`)
	// rootForm and serversForm declare the server with dotted keys at the
	// root or inline under [mcp_servers]; install does not edit those.
	rootForm    = regexp.MustCompile(`^\s*"?mcp_servers"?\s*[.=]`)
	serversForm = regexp.MustCompile(`^\s*"?crosstalk"?\s*[.=]`)
)

func tableName(h string) string {
	parts := strings.Split(h, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, ".")
}

// table is a header line and its body, up to the next header.
type table struct {
	name       string
	start, end int
}

// ponytail: line-based TOML; a multi-line string holding a line that looks
// like a header splits a table. A TOML parser is the upgrade if that bites.
func tables(lines []string) []table {
	var ts []table
	for i, l := range lines {
		if m := headerRe.FindStringSubmatch(l); m != nil {
			if len(ts) > 0 {
				ts[len(ts)-1].end = i
			}
			ts = append(ts, table{tableName(m[1]), i, len(lines)})
		}
	}
	return ts
}

func otherForm(lines []string) bool {
	ts := tables(lines)
	root := len(lines)
	if len(ts) > 0 {
		root = ts[0].start
	}
	for _, l := range lines[:root] {
		if rootForm.MatchString(l) {
			return true
		}
	}
	for _, t := range ts {
		if t.name != "mcp_servers" {
			continue
		}
		for _, l := range lines[t.start+1 : t.end] {
			if serversForm.MatchString(l) {
				return true
			}
		}
	}
	return false
}

func findTable(lines []string, name string) (table, bool) {
	for _, t := range tables(lines) {
		if t.name == name {
			return t, true
		}
	}
	return table{}, false
}

// orphanTables reports a [mcp_servers.crosstalk.*] table without its parent,
// which makes Codex refuse the whole config ("invalid transport").
func orphanTables(cfg string) bool {
	lines := strings.Split(cfg, "\n")
	if _, ok := findTable(lines, serverTable); ok {
		return false
	}
	for _, t := range tables(lines) {
		if strings.HasPrefix(t.name, serverTable+".") {
			return true
		}
	}
	return false
}

// tomlCode returns line without its comment, and the change in array
// depth from brackets outside strings.
func tomlCode(line string) (string, int) {
	depth := 0
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '"' && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return line[:i], depth
		case c == '[':
			depth++
		case c == ']':
			depth--
		}
	}
	return line, depth
}

// keySpan finds key in lines[from:to] and returns the lines its value
// covers; a multi-line array runs to its closing bracket.
func keySpan(lines []string, from, to int, key string) (int, int, bool) {
	for i := from; i < to; i++ {
		m := keyRe.FindStringSubmatch(lines[i])
		if m == nil || m[1] != key {
			continue
		}
		_, depth := tomlCode(m[2])
		j := i + 1
		for depth > 0 && j < to {
			_, d := tomlCode(lines[j])
			depth += d
			j++
		}
		return i, j, true
	}
	return 0, 0, false
}

type entry struct {
	command string
	args    []string
	raw     string
}

func readEntry(cfg string) (entry, bool) {
	lines := strings.Split(cfg, "\n")
	t, ok := findTable(lines, serverTable)
	if !ok {
		return entry{}, false
	}
	var e entry
	if i, j, ok := keySpan(lines, t.start+1, t.end, "command"); ok {
		v, _ := tomlCode(keyRe.FindStringSubmatch(lines[i])[2])
		v = strings.TrimSpace(v)
		if s, err := strconv.Unquote(v); err == nil {
			e.command = s
		} else {
			e.command = strings.Trim(v, "'")
		}
		e.raw += strings.Join(lines[i:j], "\n") + "\n"
	}
	if i, j, ok := keySpan(lines, t.start+1, t.end, "args"); ok {
		var code []string
		for _, l := range append([]string{keyRe.FindStringSubmatch(lines[i])[2]}, lines[i+1:j]...) {
			c, _ := tomlCode(l)
			code = append(code, c)
		}
		json.Unmarshal([]byte(strings.Join(code, " ")), &e.args)
		e.raw += strings.Join(lines[i:j], "\n") + "\n"
	}
	return e, true
}

func newTable(bin string) string {
	return "[mcp_servers.crosstalk]\ncommand = " + strconv.Quote(bin) + "\nargs = [\"mcp\"]\ndefault_tools_approval_mode = \"approve\"\n"
}

// editConfig points the crosstalk server at bin with args ["mcp"], keeping
// every other key and sub-table, or appends the table when there is none.
func editConfig(cfg, bin string) (string, error) {
	lines := strings.Split(cfg, "\n")
	if otherForm(lines) {
		return "", fmt.Errorf("the crosstalk server is declared as an inline table or with dotted keys, which install does not edit; replace that declaration by hand with:\n%s", newTable(bin))
	}
	t, ok := findTable(lines, serverTable)
	if !ok {
		out := cfg
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if out != "" {
			out += "\n"
		}
		return out + newTable(bin), nil
	}
	body := slices.Clone(lines[t.start+1 : t.end])
	set := func(key, line string) {
		if i, j, ok := keySpan(body, 0, len(body), key); ok {
			body = slices.Replace(body, i, j, line)
			return
		}
		body = slices.Insert(body, 0, line)
	}
	set("args", `args = ["mcp"]`)
	set("command", "command = "+strconv.Quote(bin))
	return strings.Join(slices.Concat(lines[:t.start+1], body, lines[t.end:]), "\n"), nil
}

// removeTables drops the crosstalk server table and every sub-table, and
// leaves the file ending in one newline.
func removeTables(cfg string) string {
	lines := strings.Split(cfg, "\n")
	ts := tables(lines)
	for i := len(ts) - 1; i >= 0; i-- {
		if t := ts[i]; t.name == serverTable || strings.HasPrefix(t.name, serverTable+".") {
			lines = slices.Delete(lines, t.start, t.end)
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

type registration struct {
	Transport struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	} `json:"transport"`
}

func registered(ctx context.Context, env harness.InstallEnv) (*registration, error) {
	out, err := env.Run(ctx, "codex", "mcp", "get", "crosstalk", "--json")
	if err != nil {
		return nil, err
	}
	var r registration
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("codex mcp get crosstalk --json: %w", err)
	}
	return &r, nil
}

// writeVerified writes cfg and runs Codex's own parser over it, then check;
// on any failure the previous file comes back.
func writeVerified(ctx context.Context, env harness.InstallEnv, cfg string, check func() error) error {
	path := configPath(env.Home)
	orig, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := harness.ReplaceFile(path, []byte(cfg)); err != nil {
		return err
	}
	_, err = env.Run(ctx, "codex", "mcp", "list")
	if err == nil && check != nil {
		err = check()
	}
	if err == nil {
		return nil
	}
	if existed {
		if rerr := harness.ReplaceFile(path, orig); rerr != nil {
			return errors.Join(err, rerr)
		}
	} else {
		os.Remove(path)
	}
	return fmt.Errorf("%w; %s restored", err, path)
}

func pluginStep(env harness.InstallEnv) harness.Step {
	return harness.Step{
		Title:  "Codex plugin",
		Change: "codex plugin marketplace add " + env.Marketplace + ", codex plugin marketplace upgrade crosstalk, codex plugin add " + pluginID,
		Apply: func(ctx context.Context) error {
			cmds := [][]string{{"plugin", "marketplace", "add", env.Marketplace}}
			// codex refuses upgrade on a local marketplace, which add already reads live.
			if !filepath.IsAbs(env.Marketplace) && !strings.HasPrefix(env.Marketplace, ".") {
				cmds = append(cmds, []string{"plugin", "marketplace", "upgrade", "crosstalk"})
			}
			for _, args := range append(cmds, []string{"plugin", "add", pluginID}) {
				if _, err := env.Run(ctx, "codex", args...); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func configStep(env harness.InstallEnv) harness.Step {
	path := configPath(env.Home)
	change := fmt.Sprintf("append to %s (backup next to it):\n%s", path, newTable(env.Bin))
	if b, err := os.ReadFile(path); err == nil {
		if e, ok := readEntry(string(b)); ok {
			change = fmt.Sprintf("in %s (backup next to it), [mcp_servers.crosstalk] gets command = %q and args = [\"mcp\"]; other keys and sub-tables stay", path, env.Bin)
			if filepath.Base(e.command) != "crosstalk" || !slices.Equal(e.args, []string{"mcp"}) {
				change += "\nreplaces this entry, whose args are not kept:\n" + e.raw
			}
		}
	}
	return harness.Step{
		Title:  "Codex MCP server",
		Change: change,
		Apply: func(ctx context.Context) error {
			b, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			cfg, err := editConfig(string(b), env.Bin)
			if err != nil {
				return err
			}
			return writeVerified(ctx, env, cfg, func() error {
				r, err := registered(ctx, env)
				if err != nil {
					return err
				}
				if r.Transport.Command != env.Bin || !slices.Equal(r.Transport.Args, []string{"mcp"}) {
					return fmt.Errorf("codex reads command %q args %q, want %q [mcp]", r.Transport.Command, r.Transport.Args, env.Bin)
				}
				return nil
			})
		},
	}
}

// InstallSteps adds the plugin before editing the config, since codex
// plugin add rewrites config.toml.
func (h *Harness) InstallSteps(env harness.InstallEnv) []harness.Step {
	return []harness.Step{pluginStep(env), configStep(env)}
}

// UninstallSteps removes the server table with its sub-tables, then the
// plugin; the marketplace stays.
func (h *Harness) UninstallSteps(env harness.InstallEnv) []harness.Step {
	path := configPath(env.Home)
	return []harness.Step{
		{
			Title:  "Codex MCP server",
			Change: "remove [mcp_servers.crosstalk] and every [mcp_servers.crosstalk.*] table from " + path + " (backup next to it)",
			Apply: func(ctx context.Context) error {
				b, err := os.ReadFile(path)
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				ours := func(t table) bool { return t.name == serverTable || strings.HasPrefix(t.name, serverTable+".") }
				if !slices.ContainsFunc(tables(strings.Split(string(b), "\n")), ours) {
					return nil
				}
				return writeVerified(ctx, env, removeTables(string(b)), nil)
			},
		},
		{
			Title:  "Codex plugin",
			Change: "codex plugin remove " + pluginID + " (the marketplace stays)",
			Apply: func(ctx context.Context) error {
				if _, err := os.Stat(pluginCache(env.Home)); errors.Is(err, os.ErrNotExist) {
					return nil
				}
				_, err := env.Run(ctx, "codex", "plugin", "remove", pluginID)
				return err
			},
		},
	}
}

func (h *Harness) Registered(env harness.InstallEnv) (bool, error) {
	b, err := os.ReadFile(configPath(env.Home))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if otherForm(strings.Split(string(b), "\n")) {
		return false, fmt.Errorf("%s declares the crosstalk server in a form install does not read", configPath(env.Home))
	}
	e, ok := readEntry(string(b))
	return ok && e.command == env.Bin, nil
}

func (h *Harness) SetUp(env harness.InstallEnv) (bool, error) {
	b, err := os.ReadFile(configPath(env.Home))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	if otherForm(strings.Split(string(b), "\n")) {
		return true, nil
	}
	if _, ok := readEntry(string(b)); ok || slices.ContainsFunc(tables(strings.Split(string(b), "\n")), func(t table) bool {
		return t.name == serverTable || strings.HasPrefix(t.name, serverTable+".")
	}) {
		return true, nil
	}
	_, err = os.Stat(pluginCache(env.Home))
	return err == nil, nil
}

// pluginCache holds one directory per installed version of the plugin.
func pluginCache(home string) string {
	return filepath.Join(codexHome(home), "plugins", "cache", "crosstalk", "crosstalk")
}

func pluginManifest(home, version string) string {
	return filepath.Join(pluginCache(home), version, ".codex-plugin", "plugin.json")
}

func (h *Harness) Checks(ctx context.Context, env harness.InstallEnv) []harness.CheckResult {
	k1 := harness.CheckResult{ID: "K1"}
	k2 := harness.CheckResult{ID: "K2"}
	var man struct {
		Version    string `json:"version"`
		MCPServers any    `json:"mcpServers"`
	}
	mp := pluginManifest(env.Home, env.Version)
	b, err := os.ReadFile(mp)
	if err == nil {
		err = json.Unmarshal(b, &man)
	}
	if err != nil {
		k1.Detail, k2.Detail = "plugin "+pluginID+" "+env.Version+" is not installed: "+err.Error(), "plugin manifest unreadable"
	} else {
		k1.OK, k1.Detail = man.Version == env.Version, mp+" has version "+man.Version
		k2.OK, k2.Detail = man.MCPServers == nil, "the installed plugin declares no server"
		if !k2.OK {
			k2.Detail = "the installed plugin starts its own server too; update the plugin"
		}
	}

	k3 := harness.CheckResult{ID: "K3"}
	cfg, _ := os.ReadFile(configPath(env.Home))
	if _, err := env.Run(ctx, "codex", "mcp", "list"); err != nil {
		k3.Detail = "codex rejects " + configPath(env.Home) + ": " + err.Error()
	} else if orphanTables(string(cfg)) {
		k3.Detail = "a [mcp_servers.crosstalk.*] table has no [mcp_servers.crosstalk]"
	} else {
		k3.OK, k3.Detail = true, "codex mcp list accepts the config"
	}

	k5 := harness.CheckResult{ID: "K5"}
	k6 := harness.CheckResult{ID: "K6"}
	r, err := registered(ctx, env)
	if err != nil {
		k5.Detail, k6.Detail = err.Error(), "no registered server to start"
		return []harness.CheckResult{k1, k2, k3, k5, k6}
	}
	t := r.Transport
	fi, serr := os.Stat(t.Command)
	k5.OK = t.Command == env.Bin && serr == nil && fi.Mode().Perm()&0o111 != 0 && slices.Equal(t.Args, []string{"mcp"})
	k5.Detail = fmt.Sprintf("registered %q %q, want %q [mcp], executable", t.Command, t.Args, env.Bin)
	var envs []string
	for k, v := range t.Env {
		envs = append(envs, k+"="+v)
	}
	slices.Sort(envs)
	if err := env.Handshake(ctx, t.Command, t.Args, envs); err != nil {
		k6.Detail = err.Error()
	} else {
		k6.OK, k6.Detail = true, "registered server lists the CrossTalk tools at "+env.Version
	}
	return []harness.CheckResult{k1, k2, k3, k5, k6}
}
