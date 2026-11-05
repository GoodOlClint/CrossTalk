package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const installUsage = `usage: crosstalk install   [claude|codex] [--yes] [--path] [--bin-dir DIR] [--skip-rule-port] [--check]
       crosstalk uninstall [claude|codex] [--yes] [--purge] [--bin-dir DIR]
       crosstalk reinstall [claude|codex] [--yes] [--path] [--bin-dir DIR] [--skip-rule-port]
`

type installOpts struct {
	mode                            string
	names                           []string
	yes, path, purge, check, noPort bool
	binDir                          string
}

func parseInstallArgs(mode string, args []string) (installOpts, error) {
	o := installOpts{mode: mode}
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.StringVar(&o.binDir, "bin-dir", "", "")
	if mode == "uninstall" {
		fs.BoolVar(&o.purge, "purge", false, "")
	} else {
		fs.BoolVar(&o.path, "path", false, "")
		fs.BoolVar(&o.noPort, "skip-rule-port", false, "")
	}
	if mode == "install" {
		fs.BoolVar(&o.check, "check", false, "")
	}
	for {
		if err := fs.Parse(args); err != nil {
			return o, err
		}
		if fs.NArg() == 0 {
			break
		}
		n := fs.Arg(0)
		if n != "claude" && n != "codex" {
			return o, fmt.Errorf("unknown harness %q", n)
		}
		o.names = append(o.names, n)
		args = fs.Args()[1:]
	}
	return o, nil
}

func installRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type installTarget struct {
	name string
	in   harness.Installer
}

// runInstall is install, uninstall and reinstall. tty reports that in is a
// terminal a person can answer prompts on.
func runInstall(mode string, args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	o, err := parseInstallArgs(mode, args)
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk %s: %v\n%s", mode, err, installUsage)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk %s: %v\n", mode, err)
		return 1
	}
	if o.binDir == "" {
		o.binDir = filepath.Join(home, ".local", "bin")
	}
	if o.binDir, err = filepath.Abs(o.binDir); err != nil {
		fmt.Fprintf(stderr, "crosstalk %s: %v\n", mode, err)
		return 2
	}
	stateDir, err := registry.DefaultDir()
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk %s: %v\n", mode, err)
		return 1
	}
	hs, err := harnesses(stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk %s: %v\n", mode, err)
		return 1
	}
	var all, targets []installTarget
	for _, h := range hs {
		if i, ok := h.(harness.Installer); ok {
			all = append(all, installTarget{string(h.Name()), i})
		}
	}
	slices.SortFunc(all, func(a, b installTarget) int { return strings.Compare(a.name, b.name) })
	for _, t := range all {
		named := slices.Contains(o.names, t.name)
		switch {
		case len(o.names) > 0 && !named:
		case !t.in.Found() && named:
			fmt.Fprintf(stderr, "%s: not found on PATH\n", t.name)
			return 1
		case !t.in.Found():
			fmt.Fprintf(stdout, "%s: not found on PATH, skipped\n", t.name)
		default:
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(stderr, "crosstalk: neither claude nor codex is on PATH")
		return 1
	}
	env := harness.InstallEnv{
		Bin:          filepath.Join(o.binDir, "crosstalk"),
		Version:      mcpserver.Version,
		Home:         home,
		Run:          installRun,
		SkipRulePort: o.noPort,
	}
	type group struct {
		title string
		steps []harness.Step
	}
	var groups []group
	if mode != "uninstall" {
		groups = append(groups, group{"crosstalk", []harness.Step{installBinaryStep(env.Bin)}})
	}
	for _, t := range targets {
		var steps []harness.Step
		if mode == "uninstall" {
			steps = t.in.UninstallSteps(env)
		} else {
			steps = t.in.InstallSteps(env)
		}
		groups = append(groups, group{t.name, steps})
	}
	if !tty && !o.yes {
		for _, g := range groups {
			fmt.Fprintf(stdout, "== %s\n", g.title)
			for _, s := range g.steps {
				fmt.Fprintf(stdout, "- %s\n%s\n", s.Title, installIndent(s.Change))
			}
		}
		fmt.Fprintf(stderr, "crosstalk %s: no terminal to confirm on; nothing changed. Run it in a terminal or pass --yes.\n", mode)
		return 2
	}
	ctx := context.Background()
	answers := bufio.NewReader(in)
	for _, g := range groups {
		fmt.Fprintf(stdout, "== %s\n", g.title)
		for _, s := range g.steps {
			fmt.Fprintf(stdout, "- %s\n%s\n", s.Title, installIndent(s.Change))
			if !o.yes && !installConfirm(answers, stdout) {
				fmt.Fprintln(stderr, "stopped: step declined")
				return 1
			}
			if err := s.Apply(ctx); err != nil {
				fmt.Fprintf(stderr, "%s: %s: %v\n", g.title, s.Title, err)
				return 1
			}
		}
	}
	return 0
}

func installConfirm(r *bufio.Reader, w io.Writer) bool {
	fmt.Fprint(w, "  apply? [y/N] ")
	line, _ := r.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func installIndent(s string) string {
	if s == "" {
		return ""
	}
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

var installExecutable = os.Executable

// installBinary copies src to dst through a temp file renamed over dst, so a
// process running the old dst keeps its inode: writing a running binary in
// place fails with ETXTBSY on Linux and gets a signed binary killed on macOS.
func installBinary(src, dst string) (skipped bool, err error) {
	src, err = filepath.EvalSymlinks(src)
	if err != nil {
		return false, err
	}
	si, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	if di, err := os.Stat(dst); err == nil && os.SameFile(si, di) {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".crosstalk-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Chmod(0o755)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	return false, err
}

func installBinaryStep(bin string) harness.Step {
	src, err := installExecutable()
	if err == nil {
		src, err = filepath.EvalSymlinks(src)
	}
	return harness.Step{
		Title:  "binary",
		Change: fmt.Sprintf("copy %s to %s (mode 0755); the source is left in place", src, bin),
		Apply: func(context.Context) error {
			if err != nil {
				return err
			}
			_, err := installBinary(src, bin)
			return err
		},
	}
}
