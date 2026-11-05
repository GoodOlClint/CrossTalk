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

// keySpan finds key in lines[from:to] and returns the lines its value
// covers; a multi-line array runs to its closing bracket.
func keySpan(lines []string, from, to int, key string) (int, int, bool) {
	for i := from; i < to; i++ {
		m := keyRe.FindStringSubmatch(lines[i])
		if m == nil || m[1] != key {
			continue
		}
		depth := strings.Count(m[2], "[") - strings.Count(m[2], "]")
		j := i + 1
		for depth > 0 && j < to {
			depth += strings.Count(lines[j], "[") - strings.Count(lines[j], "]")
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
		v := strings.TrimSpace(keyRe.FindStringSubmatch(lines[i])[2])
		if s, err := strconv.Unquote(v); err == nil {
			e.command = s
		} else {
			e.command = strings.Trim(v, "'")
		}
		e.raw += strings.Join(lines[i:j], "\n") + "\n"
	}
	if i, j, ok := keySpan(lines, t.start+1, t.end, "args"); ok {
		v := keyRe.FindStringSubmatch(lines[i])[2]
		json.Unmarshal([]byte(strings.Join(append([]string{v}, lines[i+1:j]...), " ")), &e.args)
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
		Change: "codex plugin marketplace add GoodOlClint/CrossTalk, codex plugin marketplace upgrade crosstalk, codex plugin add " + pluginID,
		Apply: func(ctx context.Context) error {
			for _, args := range [][]string{
				{"plugin", "marketplace", "add", "GoodOlClint/CrossTalk"},
				{"plugin", "marketplace", "upgrade", "crosstalk"},
				{"plugin", "add", pluginID},
			} {
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

func (h *Harness) UninstallSteps(env harness.InstallEnv) []harness.Step { return nil }

func (h *Harness) Checks(ctx context.Context, env harness.InstallEnv) []harness.CheckResult {
	return nil
}
