package opencode

import (
	"context"
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

var versionOut = regexp.MustCompile(`\d+\.\d+\.\d+`)

func (h *Harness) Found() bool {
	_, err := exec.LookPath("opencode")
	return err == nil
}

func configDir(home string) string {
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "opencode")
	}
	return filepath.Join(home, ".config", "opencode")
}

// globalFiles lists the global config candidates in the order the OpenCode
// CLI writes them: opencode.json when it exists, else opencode.jsonc, else a
// new opencode.json.
func globalFiles(home string) []string {
	d := configDir(home)
	return []string{filepath.Join(d, "opencode.json"), filepath.Join(d, "opencode.jsonc")}
}

func writeTarget(home string) string {
	fs := globalFiles(home)
	for _, f := range fs {
		if _, err := os.Stat(f); err == nil {
			return f
		}
	}
	return fs[0]
}

func loadFile(path string) (*config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseConfig(path, b)
}

func loadGlobal(home string) ([]*config, error) {
	var out []*config
	for _, f := range globalFiles(home) {
		c, err := loadFile(f)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func entriesOf(cs []*config) []entry {
	var es []entry
	for _, c := range cs {
		es = append(es, c.entries...)
	}
	return es
}

func runsBin(e entry, bin string) bool {
	return len(e.command) > 0 && sameBin(e.command[0], bin)
}

func ownEntry(e entry, bin string) bool {
	return runsBin(e, bin) && slices.Equal(e.command[1:], []string{"mcp"})
}

func envWanted(e entry, env harness.InstallEnv) bool {
	return !env.NoAutoRefresh || e.env[harness.OffEnv] == "1"
}

func checkInstallable(ctx context.Context, env harness.InstallEnv) error {
	out, err := env.Run(ctx, "opencode", "--version")
	if err != nil {
		return fmt.Errorf("opencode --version: %w", err)
	}
	v := versionOut.FindString(string(out))
	if v == "" {
		return fmt.Errorf("opencode --version printed no version: %q", strings.TrimSpace(string(out)))
	}
	if major, _, _ := strings.Cut(v, "."); major == "1" {
		return errors.New("OpenCode 1.x found: unsupported, see ADR 0032; nothing was written")
	}
	warn, err := CheckVersion(v)
	if err != nil {
		return fmt.Errorf("OpenCode %s: %w; nothing was written", v, err)
	}
	if warn != "" && env.Out != nil {
		fmt.Fprintln(env.Out, warn)
	}
	return nil
}

func addArgs(env harness.InstallEnv) []string {
	args := []string{"mcp", "add", "crosstalk", "--global"}
	if env.NoAutoRefresh {
		args = append(args, "--env", harness.OffEnv+"=1")
	}
	return append(args, "--", env.Bin, "mcp")
}

type snapshot struct {
	path    string
	content []byte
	existed bool
}

func snapshotGlobal(home string) ([]snapshot, error) {
	var out []snapshot
	for _, f := range globalFiles(home) {
		b, err := os.ReadFile(f)
		switch {
		case err == nil:
			out = append(out, snapshot{f, b, true})
		case errors.Is(err, os.ErrNotExist):
			out = append(out, snapshot{path: f})
		default:
			return nil, err
		}
	}
	return out, nil
}

func restore(snaps []snapshot) error {
	var errs []error
	for _, s := range snaps {
		cur, err := os.ReadFile(s.path)
		switch {
		case !s.existed && errors.Is(err, os.ErrNotExist):
		case !s.existed:
			errs = append(errs, os.Remove(s.path))
		case err != nil || string(cur) != string(s.content):
			errs = append(errs, harness.ReplaceFile(s.path, s.content))
		}
	}
	return errors.Join(errs...)
}

func (h *Harness) configStep(env harness.InstallEnv) harness.Step {
	path := writeTarget(env.Home)
	change := fmt.Sprintf("run opencode %s: it adds mcp.servers.crosstalk to %s (a backup next to it; comments and other keys stay)", strings.Join(addArgs(env), " "), path)
	return harness.Step{
		Title:  "OpenCode MCP server",
		Change: change,
		Apply: func(ctx context.Context) error {
			if err := checkInstallable(ctx, env); err != nil {
				return err
			}
			cs, err := loadGlobal(env.Home)
			if err != nil {
				return err
			}
			var own []entry
			for _, e := range entriesOf(cs) {
				if !ownEntry(e, env.Bin) {
					return fmt.Errorf("%s has a crosstalk entry (%s) that runs %q, not %s; install changed nothing: edit or remove that entry by hand, then run install again", e.file, e.shape, strings.Join(e.command, " "), env.Bin)
				}
				own = append(own, e)
			}
			if len(own) > 1 {
				return fmt.Errorf("%s and %s both name crosstalk (%s and %s); install changed nothing: remove one by hand, then run install again", own[0].file, own[1].file, own[0].shape, own[1].shape)
			}
			if len(own) == 1 && envWanted(own[0], env) {
				return nil
			}
			if len(own) == 1 && own[0].shape == shapeV1 {
				return fmt.Errorf("%s has the %s entry without environment %s=1, which install does not edit; add it by hand", own[0].file, shapeV1, harness.OffEnv)
			}
			snaps, err := snapshotGlobal(env.Home)
			if err != nil {
				return err
			}
			target := writeTarget(env.Home)
			for _, s := range snaps {
				if s.path == target && s.existed {
					if err := harness.ReplaceFile(target, s.content); err != nil {
						return err
					}
				}
			}
			fail := func(err error) error {
				if rerr := restore(snaps); rerr != nil {
					return errors.Join(err, rerr)
				}
				return fmt.Errorf("%w; the OpenCode config is restored", err)
			}
			if _, err := env.Run(ctx, "opencode", addArgs(env)...); err != nil {
				return fail(fmt.Errorf("opencode mcp add: %w", err))
			}
			after, err := loadGlobal(env.Home)
			if err != nil {
				return fail(err)
			}
			es := entriesOf(after)
			if len(es) != 1 || !ownEntry(es[0], env.Bin) || es[0].shape != shapeV2 {
				return fail(fmt.Errorf("opencode wrote %d crosstalk entries, want one %s running %s mcp", len(es), shapeV2, env.Bin))
			}
			if !envWanted(es[0], env) {
				return fail(fmt.Errorf("opencode wrote the entry without environment %s=1", harness.OffEnv))
			}
			return nil
		},
	}
}

func (h *Harness) InstallSteps(env harness.InstallEnv) []harness.Step {
	return []harness.Step{h.configStep(env)}
}

func (h *Harness) UninstallSteps(env harness.InstallEnv) []harness.Step {
	return []harness.Step{{
		Title:  "OpenCode MCP server",
		Change: "remove the crosstalk entry that runs " + env.Bin + " from " + filepath.Join(configDir(env.Home), "opencode.json[c]") + " (a backup next to it); a file with comments is not rewritten, and the lines to delete are printed",
		Apply: func(ctx context.Context) error {
			cs, err := loadGlobal(env.Home)
			if err != nil {
				return err
			}
			for _, c := range cs {
				var ours []entry
				for _, e := range c.entries {
					if runsBin(e, env.Bin) {
						ours = append(ours, e)
					}
				}
				if len(ours) == 0 {
					continue
				}
				var spans []span
				for _, e := range ours {
					sp, ok := memberSpan(c.clean, 0, e.path)
					if !ok {
						return fmt.Errorf("%s: cannot locate the %s entry", c.path, e.shape)
					}
					spans = append(spans, sp)
				}
				if c.comments {
					var lines []string
					for _, sp := range spans {
						lines = append(lines, lineSpan(c.src, sp))
					}
					return fmt.Errorf("%s has comments, so uninstall does not rewrite it (a rewrite would drop them); delete the crosstalk entry by hand, these lines:\n%s", c.path, strings.Join(lines, "\n"))
				}
				out := cutMembers(c.src, spans)
				nc, err := parseConfig(c.path, out)
				if err != nil || slices.ContainsFunc(nc.entries, func(e entry) bool { return runsBin(e, env.Bin) }) {
					return fmt.Errorf("%s: removing the entry would not leave a valid config (%v); delete it by hand", c.path, err)
				}
				if err := harness.ReplaceFile(c.path, out); err != nil {
					return err
				}
			}
			return nil
		},
	}}
}

func (h *Harness) Registered(env harness.InstallEnv) (bool, error) {
	cs, err := loadGlobal(env.Home)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(entriesOf(cs), func(e entry) bool { return runsBin(e, env.Bin) }), nil
}

func (h *Harness) SetUp(env harness.InstallEnv) (bool, error) {
	cs, err := loadGlobal(env.Home)
	if err != nil {
		return true, err
	}
	return len(entriesOf(cs)) > 0, nil
}

func projectFiles() []string {
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(wd, "opencode.json"), filepath.Join(wd, "opencode.jsonc")}
}

func sameFile(a, b string) bool {
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
}

func (h *Harness) Checks(ctx context.Context, env harness.InstallEnv) []harness.CheckResult {
	k2, k5, k6 := harness.CheckResult{ID: "K2"}, harness.CheckResult{ID: "K5"}, harness.CheckResult{ID: "K6"}
	var es []entry
	cs, err := loadGlobal(env.Home)
	if err == nil {
		es = entriesOf(cs)
		for _, f := range projectFiles() {
			if slices.ContainsFunc(globalFiles(env.Home), func(g string) bool { return sameFile(f, g) }) {
				continue
			}
			var pc *config
			if pc, err = loadFile(f); err != nil {
				break
			}
			if pc != nil {
				es = append(es, pc.entries...)
			}
		}
	}
	switch {
	case err != nil:
		k2.Detail = "cannot read the OpenCode config: " + err.Error()
	case len(es) == 1:
		k2.OK, k2.Detail = true, "one CrossTalk entry, "+es[0].file+" ("+es[0].shape+")"
	case len(es) == 0:
		k2.Detail = "no CrossTalk entry in the global or project OpenCode config; run crosstalk install opencode"
	default:
		var where []string
		for _, e := range es {
			where = append(where, e.file+" ("+e.shape+")")
		}
		k2.Detail = fmt.Sprintf("%d CrossTalk entries, want one: %s", len(es), strings.Join(where, ", "))
	}

	if len(es) == 0 {
		k5.Detail, k6.Detail = "no registered server", "no registered server to start"
	} else {
		e := es[0]
		for _, c := range es {
			if runsBin(c, env.Bin) {
				e = c
				break
			}
		}
		fi, serr := os.Stat(e.command[0])
		k5.OK = ownEntry(e, env.Bin) && serr == nil && fi.Mode().Perm()&0o111 != 0
		k5.Detail = fmt.Sprintf("registered %q, want %q [mcp], executable", e.command, env.Bin)
		var envs []string
		for k, v := range e.env {
			envs = append(envs, k+"="+v)
		}
		slices.Sort(envs)
		if err := env.Handshake(ctx, e.command[0], e.command[1:], envs); err != nil {
			k6.Detail = err.Error()
		} else {
			k6.OK, k6.Detail = true, "registered server lists the CrossTalk tools at "+env.Version
		}
	}
	return []harness.CheckResult{k2, k5, k6, h.k13()}
}

func (h *Harness) k13() harness.CheckResult {
	r := harness.CheckResult{ID: "K13", WarnOnly: true}
	svc, err := ReadService(h.client.ServicePath)
	var se *ServiceError
	switch {
	case errors.As(err, &se) && se.Reason == ReasonMissing:
		r.OK, r.Detail = true, "OpenCode service not running (service.json absent); not checked"
		return r
	case err != nil:
		r.Detail = err.Error()
		return r
	}
	warn, verr := CheckVersion(svc.Version)
	switch {
	case verr != nil && strings.HasPrefix(svc.Version, "1."):
		r.Detail = "OpenCode 1.x found: unsupported, see ADR 0032"
	case verr != nil:
		r.Detail = "OpenCode service version " + strconv.Quote(svc.Version) + " is not tested: " + verr.Error()
	case warn != "":
		r.Detail = warn
	default:
		r.OK, r.Detail = true, "OpenCode service "+svc.Version+" on loopback, service.json mode 0600"
	}
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
