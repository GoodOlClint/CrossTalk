package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

var (
	_ harness.Installer     = (*Harness)(nil)
	_ harness.PluginUpdater = (*Harness)(nil)
	_ harness.Leftover      = (*Harness)(nil)
)

// AgentDirEnv relocates Pi's agent directory.
const AgentDirEnv = "PI_CODING_AGENT_DIR"

// versionTimeout bounds `pi --version`; a test shortens it.
var versionTimeout = 5 * time.Second

// AgentDir is Pi's agent directory: $PI_CODING_AGENT_DIR, with a leading ~
// expanded as Pi does, else ~/.pi/agent.
func AgentDir(home string) string {
	d := os.Getenv(AgentDirEnv)
	switch {
	case d == "":
		return filepath.Join(home, ".pi", "agent")
	case d == "~":
		return filepath.Clean(home)
	case strings.HasPrefix(d, "~/"):
		return filepath.Join(home, d[2:])
	}
	return filepath.Clean(d)
}

// agentDir refuses a relative value: Pi resolves it against each session's
// working directory, so no one file serves it.
func agentDir(home string) (string, error) {
	d := AgentDir(home)
	if !filepath.IsAbs(d) {
		return "", fmt.Errorf("%s is %q, not an absolute path or ~/..., so the extension file has no one place; set an absolute path", AgentDirEnv, os.Getenv(AgentDirEnv))
	}
	return d, nil
}

func extPath(dir string) string { return filepath.Join(dir, "extensions", "crosstalk", "index.ts") }

const backupSuffix = ".crosstalk-backup"

var (
	binLine     = regexp.MustCompile(`(?m)^const BIN = (".*");$`)
	versionLine = regexp.MustCompile(`(?m)^const VERSION_TAG = (".*");$`)
)

// extension is what the file at path holds. The values are read only from an
// owned file, so a foreign file's command is never started.
type extension struct {
	content          []byte // nil when there is no file
	owned, noRefresh bool
	bin, version     string
}

func readExtension(path string) (extension, error) {
	var x extension
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return x, nil
	}
	if err != nil {
		return x, err
	}
	if !fi.Mode().IsRegular() {
		return x, fmt.Errorf("%s is not a regular file", path)
	}
	if x.content, err = os.ReadFile(path); err != nil {
		return x, err
	}
	x.owned = bytes.HasPrefix(x.content, []byte(marker+"\n"))
	if !x.owned {
		return x, nil
	}
	x.noRefresh = bytes.Contains(x.content, []byte(refreshOff))
	unquote := func(re *regexp.Regexp) (s string) {
		if m := re.FindSubmatch(x.content); m != nil {
			_ = json.Unmarshal(m[1], &s)
		}
		return s
	}
	x.bin, x.version = unquote(binLine), unquote(versionLine)
	return x, nil
}

func (h *Harness) installed(home string) (string, extension, error) {
	dir, err := agentDir(home)
	if err != nil {
		return "", extension{}, err
	}
	path := extPath(dir)
	x, err := readExtension(path)
	return path, x, err
}

func refuseLink(path string) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink, which install and uninstall do not replace; edit its target by hand", path)
	}
	return nil
}

// shadowed refuses a directory Pi would not load the written index.ts from,
// or whose own entry point the new file would replace. have reports that
// CrossTalk's index.ts is already there.
func shadowed(dir string, have bool) error {
	names := []string{"package.json"}
	if !have {
		names = append(names, "index.js")
	}
	for _, n := range names {
		if _, err := os.Lstat(filepath.Join(dir, n)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s exists, which CrossTalk did not write and which changes what Pi loads from %s; move it away, then run install again", filepath.Join(dir, n), dir)
		}
	}
	return nil
}

// linked refuses a symlinked extension directory or file.
func linked(path string) error {
	return errors.Join(refuseLink(filepath.Dir(path)), refuseLink(path))
}

// mcpFiles are the global mcp.json and the working directory's project one.
func mcpFiles(dir string) []string {
	files := []string{filepath.Join(dir, "mcp.json")}
	if wd, err := os.Getwd(); err == nil {
		files = append(files, filepath.Join(wd, ".pi", "mcp.json"))
	}
	return files
}

// readJSON parses path into v, past a byte-order mark as Pi does.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\ufeff")), v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

type mcpEntry struct{ file, command string }

// mcpEntries lists the crosstalk servers in both mcp.json files.
func mcpEntries(dir string) ([]mcpEntry, error) {
	var out []mcpEntry
	for _, f := range mcpFiles(dir) {
		var c struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		}
		err := readJSON(f, &c)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		raw, ok := c.Servers["crosstalk"]
		if !ok || slices.ContainsFunc(out, func(e mcpEntry) bool { return e.file == f }) {
			continue
		}
		var server struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(raw, &server)
		out = append(out, mcpEntry{f, server.Command})
	}
	return out, nil
}

// removal names each entry's file and the command that removes it.
func removal(dir string, entries []mcpEntry) string {
	var out []string
	for _, e := range entries {
		cmd := "pi mcp remove crosstalk"
		if e.file != filepath.Join(dir, "mcp.json") {
			cmd += " --local, in that project"
		}
		out = append(out, e.file+" (run `"+cmd+"`)")
	}
	return strings.Join(out, " and ")
}

func piVersion(ctx context.Context, env harness.InstallEnv) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := env.Run(ctx, "pi", "--version")
	if err != nil {
		return "", fmt.Errorf("pi --version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *Harness) Found() bool {
	_, err := exec.LookPath("pi")
	return err == nil
}

func checkInstallable(ctx context.Context, env harness.InstallEnv) error {
	v, err := piVersion(ctx, env)
	if err != nil {
		return fmt.Errorf("%w; no Pi file was written", err)
	}
	warn, err := CheckVersion(v)
	if err != nil {
		return fmt.Errorf("%w; no Pi file was written", err)
	}
	if warn != "" && env.Out != nil {
		fmt.Fprintln(env.Out, warn)
	}
	dir, err := agentDir(env.Home)
	if err != nil {
		return err
	}
	entries, err := mcpEntries(dir)
	if err != nil {
		return fmt.Errorf("%w; no Pi file was written", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("a crosstalk server entry takes precedence over the extension's; no Pi file was written: remove %s, then run install again", removal(dir, entries))
	}
	return nil
}

// PluginStep writes the extension from the embedded copy, keeping the file's
// no-auto-refresh choice. It never replaces a file without the marker.
func (h *Harness) PluginStep(env harness.InstallEnv) harness.Step {
	return harness.Step{
		Title:  "Pi extension",
		Change: "write " + extPath(AgentDir(env.Home)) + ", which registers the CrossTalk MCP server at " + env.Bin + " in each Pi session (a backup next to a file it replaces)",
		Apply: func(context.Context) error {
			path, x, err := h.installed(env.Home)
			if err != nil {
				return err
			}
			if err := linked(path); err != nil {
				return err
			}
			if x.content != nil && !x.owned {
				return fmt.Errorf("%s exists and is not CrossTalk's (no marker line), so it is left as it is; move it away, then run install again", path)
			}
			if err := shadowed(filepath.Dir(path), x.owned); err != nil {
				return err
			}
			next := Fill(env.Bin, env.Version, env.NoAutoRefresh, x.content)
			if bytes.Equal(next, x.content) {
				return nil
			}
			if x.content != nil {
				if err := refuseLink(path + backupSuffix); err != nil {
					return err
				}
				if b, err := readExtension(path + backupSuffix); err != nil {
					return err
				} else if b.content != nil && !b.owned {
					return fmt.Errorf("%s%s exists and is not CrossTalk's, so it is not overwritten; move it away, then run install again", path, backupSuffix)
				}
			}
			return harness.ReplaceFile(path, next)
		},
	}
}

func (h *Harness) InstallSteps(env harness.InstallEnv) []harness.Step {
	step := h.PluginStep(env)
	write := step.Apply
	step.Apply = func(ctx context.Context) error {
		if err := checkInstallable(ctx, env); err != nil {
			return err
		}
		return write(ctx)
	}
	return []harness.Step{step}
}

// ownedFile reports that path is a regular file with the marker, reached
// through no link in the extension's own directory.
func ownedFile(path string) (extension, bool) {
	if linked(path) != nil {
		return extension{}, false
	}
	x, err := readExtension(path)
	return x, err == nil && x.owned
}

func (h *Harness) UninstallSteps(env harness.InstallEnv) []harness.Step {
	path := extPath(AgentDir(env.Home))
	return []harness.Step{{
		Title:  "Pi extension",
		Change: "delete " + path + " (when it is CrossTalk's and runs " + env.Bin + "), its " + backupSuffix + ", and the directory if that leaves it empty",
		Apply: func(context.Context) error {
			if _, err := agentDir(env.Home); err != nil {
				return err
			}
			out := env.Out
			if out == nil {
				out = io.Discard
			}
			dir := filepath.Dir(path)
			removed := false
			if err := linked(path); err != nil {
				// Failing here keeps the binary a linked extension still runs.
				if x, rerr := readExtension(path); rerr == nil && x.owned && sameBin(x.bin, env.Bin) {
					return err
				}
				fmt.Fprintf(out, "%s is kept: %v\n", path, err)
				return nil
			}
			if _, err := os.Lstat(path); err == nil {
				if x, ok := ownedFile(path); !ok || !sameBin(x.bin, env.Bin) {
					fmt.Fprintf(out, "%s is kept: it is not CrossTalk's file for %s\n", path, env.Bin)
					return nil
				}
				if err := os.Remove(path); err != nil {
					return err
				}
				removed = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if x, ok := ownedFile(path + backupSuffix); ok && (removed || sameBin(x.bin, env.Bin)) {
				if err := os.Remove(path + backupSuffix); err != nil {
					return err
				}
				removed = true
			}
			if !removed {
				return nil
			}
			left, err := os.ReadDir(dir)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(left) == 0 {
				return os.Remove(dir)
			}
			var names []string
			for _, e := range left {
				names = append(names, e.Name())
			}
			fmt.Fprintf(out, "%s is kept: it holds %s, which CrossTalk did not write\n", dir, strings.Join(names, ", "))
			return nil
		},
	}}
}

func (h *Harness) Registered(env harness.InstallEnv) (bool, error) {
	_, x, err := h.installed(env.Home)
	return x.owned && sameBin(x.bin, env.Bin), err
}

func (h *Harness) SetUp(env harness.InstallEnv) (bool, error) {
	_, x, err := h.installed(env.Home)
	return x.owned, err
}

// Leftover reports mcp.json servers, which uninstall does not edit.
func (h *Harness) Leftover(env harness.InstallEnv, anyBin bool) (string, error) {
	dir, err := agentDir(env.Home)
	if err != nil {
		return "", err
	}
	entries, err := mcpEntries(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if anyBin || sameBin(e.command, env.Bin) {
			return "a crosstalk entry in " + e.file, nil
		}
	}
	return "", nil
}

// missing says why there is no owned extension to check, or "".
func missing(path string, x extension, err error) string {
	switch {
	case err != nil:
		return "cannot read the Pi extension: " + err.Error()
	case x.content == nil:
		return "no CrossTalk extension at " + path + "; run crosstalk install pi"
	case !x.owned:
		return path + " is not CrossTalk's (no marker line)"
	}
	return ""
}

func (h *Harness) PluginCheck(_ context.Context, env harness.InstallEnv) harness.CheckResult {
	r := harness.CheckResult{ID: "K5"}
	path, x, err := h.installed(env.Home)
	if r.Detail = missing(path, x, err); r.Detail != "" {
		return r
	}
	fi, serr := os.Stat(x.bin)
	switch {
	case !sameBin(x.bin, env.Bin) || serr != nil || fi.Mode().Perm()&0o111 == 0:
		r.Detail = fmt.Sprintf("%s runs %q, want %q, executable", path, x.bin, env.Bin)
	case x.version != env.Version:
		r.Detail = fmt.Sprintf("%s is version %q, the binary is %q; run crosstalk install pi", path, x.version, env.Version)
	default:
		r.OK, r.Detail = true, path+" runs "+env.Bin+" at "+env.Version
	}
	return r
}

func (h *Harness) Checks(ctx context.Context, env harness.InstallEnv) []harness.CheckResult {
	path, x, err := h.installed(env.Home)
	why := missing(path, x, err)

	k2 := harness.CheckResult{ID: "K2", Detail: why}
	if why == "" {
		entries, err := mcpEntries(AgentDir(env.Home))
		shadow := shadowed(filepath.Dir(path), true)
		switch {
		case shadow != nil:
			k2.Detail = shadow.Error()
		case err != nil:
			k2.Detail = "cannot read the Pi MCP config: " + err.Error()
		case len(entries) > 0:
			k2.Detail = "a crosstalk entry takes precedence over the extension's; remove " + removal(AgentDir(env.Home), entries)
		default:
			k2.OK, k2.Detail = true, "one CrossTalk server, registered by "+path
		}
	}

	k6 := harness.CheckResult{ID: "K6"}
	switch {
	case why != "":
		k6.Detail = "no registered server to start"
	case !sameBin(x.bin, env.Bin):
		k6.Detail = "not started: the registered command is not the installed binary"
	default:
		var envs []string
		if x.noRefresh {
			envs = []string{harness.OffEnv + "=1"}
		}
		if err := env.Handshake(ctx, x.bin, []string{"mcp"}, envs); err != nil {
			k6.Detail = err.Error()
		} else {
			k6.OK, k6.Detail = true, "registered server lists the CrossTalk tools at "+env.Version
		}
	}
	return []harness.CheckResult{k2, h.PluginCheck(ctx, env), k6, h.k14(ctx, env)}
}

func (h *Harness) k14(ctx context.Context, env harness.InstallEnv) harness.CheckResult {
	r := harness.CheckResult{ID: "K14", WarnOnly: true}
	var warns []string
	v, err := piVersion(ctx, env)
	if err == nil {
		var warn string
		if warn, err = CheckVersion(v); warn != "" {
			warns = append(warns, warn)
		}
	}
	if err != nil {
		warns = append(warns, err.Error())
	}
	dir, err := agentDir(env.Home)
	if err != nil {
		r.Detail = strings.Join(append(warns, err.Error()), "; ")
		return r
	}
	settings := filepath.Join(dir, "settings.json")
	var s struct {
		Extensions []string `json:"extensions"`
	}
	err = readJSON(settings, &s)
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		warns = append(warns, "cannot read "+settings+": "+err.Error())
	case slices.Contains(s.Extensions, "-builtin:mcp"):
		warns = append(warns, settings+" disables builtin:mcp (\"-builtin:mcp\" in extensions), so Pi never starts the server")
	}
	if x, err := readExtension(extPath(dir)); err == nil && x.owned && x.version != env.Version {
		warns = append(warns, fmt.Sprintf("the extension is version %q, the binary is %q", x.version, env.Version))
	}
	from := ""
	if os.Getenv(AgentDirEnv) != "" {
		from = " (from $" + AgentDirEnv + ")"
	}
	if len(warns) == 0 {
		r.OK, r.Detail = true, "Pi "+v+", builtin:mcp not disabled, agent dir "+dir+from
		return r
	}
	r.Detail = strings.Join(warns, "; ") + "; agent dir " + dir + from
	return r
}

func sameBin(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
