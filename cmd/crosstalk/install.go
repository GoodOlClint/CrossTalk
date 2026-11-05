package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
		Handshake:    installHandshake,
		SkipRulePort: o.noPort,
		Marketplace:  cmp.Or(os.Getenv("CROSSTALK_MARKETPLACE"), "GoodOlClint/CrossTalk"),
	}
	ctx := context.Background()
	if o.check {
		if installChecks(ctx, stdout, targets, env, len(o.names) == 0) {
			return 1
		}
		return 0
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
	if mode == "uninstall" {
		groups = append(groups, group{"crosstalk", installRemoveSteps(o, all, targets, env, stateDir, stdout)})
	}
	if !tty && !o.yes {
		for _, g := range groups {
			fmt.Fprintf(stdout, "== %s\n", g.title)
			for _, s := range g.steps {
				fmt.Fprintf(stdout, "- %s\n%s\n", s.Title, installIndent(s.Change))
			}
		}
		if files := installPathFiles(runtime.GOOS, home); o.path && len(files) > 0 {
			s := installPathStep(o.binDir, files)
			fmt.Fprintf(stdout, "- %s\n%s\n", s.Title, installIndent(s.Change))
		}
		fmt.Fprintf(stderr, "crosstalk %s: no terminal to confirm on; nothing changed. Run it in a terminal or pass --yes.\n", mode)
		return 2
	}
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
	if mode == "uninstall" {
		return 0
	}
	files := installPathFiles(runtime.GOOS, home)
	if len(files) > 0 && (o.path || !o.yes) && !installOnLoginPath(o.binDir) {
		s := installPathStep(o.binDir, files)
		fmt.Fprintf(stdout, "- %s\n%s\n", s.Title, installIndent(s.Change))
		if o.path && o.yes || installConfirm(answers, stdout) {
			if err := s.Apply(ctx); err != nil {
				fmt.Fprintf(stderr, "PATH: %v\n", err)
				return 1
			}
		}
	}
	failed := installChecks(ctx, stdout, targets, env, false)
	fmt.Fprintln(stdout, "Running Claude and Codex sessions keep their old CrossTalk server. Restart them (in Claude, /mcp and reconnect also works).")
	if failed {
		return 1
	}
	return 0
}

// installStillRegistered names the harnesses outside targets whose config
// still runs env.Bin, whether or not their CLI is on PATH. An error means a
// config could not be read, so the binary and state must stay.
func installStillRegistered(all, targets []installTarget, env harness.InstallEnv) ([]string, error) {
	var out []string
	var errs []error
	for _, t := range all {
		if slices.ContainsFunc(targets, func(x installTarget) bool { return x.name == t.name }) {
			continue
		}
		ok, err := t.in.Registered(env)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.name, err))
		}
		if ok {
			out = append(out, t.name)
		}
	}
	return out, errors.Join(errs...)
}

// installKeep reports why the binary and state stay, or "" when nothing
// else runs the binary.
func installKeep(all, targets []installTarget, env harness.InstallEnv) string {
	still, err := installStillRegistered(all, targets, env)
	switch {
	case err != nil:
		return "cannot tell whether another harness still runs it: " + err.Error()
	case len(still) > 0:
		return strings.Join(still, ", ") + " still runs it"
	}
	return ""
}

// installRemoveSteps delete the binary and, with --purge, the state dir,
// each only when no other harness still runs the binary.
func installRemoveSteps(o installOpts, all, targets []installTarget, env harness.InstallEnv, stateDir string, w io.Writer) []harness.Step {
	steps := []harness.Step{{
		Title:  "binary",
		Change: "delete " + env.Bin + " and the PATH lines install marked, unless another harness still runs it",
		Apply: func(ctx context.Context) error {
			if why := installKeep(all, targets, env); why != "" {
				fmt.Fprintf(w, "    kept %s: %s\n", env.Bin, why)
				return nil
			}
			if err := installRemovePath(installPathFiles(runtime.GOOS, env.Home), installPathLine(filepath.Dir(env.Bin))); err != nil {
				return err
			}
			if err := os.Remove(env.Bin); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		},
	}}
	if !o.purge {
		return steps
	}
	return append(steps, harness.Step{
		Title:  "state",
		Change: "delete " + stateDir + " (journals, tokens, descriptors) unless another harness is still installed or a shim is running",
		Apply: func(ctx context.Context) error {
			if why := installKeep(all, targets, env); why != "" {
				fmt.Fprintf(w, "    kept %s: %s\n", stateDir, why)
				return nil
			}
			held, err := registry.New(stateDir).HeldAgents()
			if err != nil {
				return err
			}
			if len(held) > 0 {
				return fmt.Errorf("running shims hold agents %s in %s; end those Claude and Codex sessions, then run uninstall --purge again", strings.Join(held, ", "), stateDir)
			}
			return os.RemoveAll(stateDir)
		},
	})
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

var installTools = []string{"list_agents", "send_message", "reply", "ask", "receive_message", "claim", "release"}

// installHandshake starts command with args, the caller's environment plus
// env, runs initialize and tools/list, and closes stdin. No tools/call is
// sent, so no agent registers.
func installHandshake(ctx context.Context, command string, args, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), env...)
	c := mcp.NewClient(&mcp.Implementation{Name: "crosstalk-install", Version: mcpserver.Version}, nil)
	s, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("%s %s: %w", command, strings.Join(args, " "), err)
	}
	defer s.Close()
	if v := s.InitializeResult().ServerInfo.Version; v != mcpserver.Version {
		return fmt.Errorf("server version %s, want %s", v, mcpserver.Version)
	}
	lt, err := s.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	var names []string
	for _, t := range lt.Tools {
		names = append(names, t.Name)
	}
	for _, n := range installTools {
		if !slices.Contains(names, n) {
			return fmt.Errorf("tools %v lack %s", names, n)
		}
	}
	return nil
}

func installPathCheck(bin string) harness.CheckResult {
	r := harness.CheckResult{ID: "K7", WarnOnly: true}
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	out, _ := exec.Command(sh, "-lc", "command -v crosstalk").Output()
	got := strings.TrimSpace(string(out))
	switch {
	case got == bin:
		r.OK, r.Detail = true, "a login shell finds "+bin
	case got == "":
		r.Detail = "a login shell does not find crosstalk; only needed for typing crosstalk send (install --path adds it)"
	default:
		r.Detail = "a login shell finds " + got + ", not " + bin
	}
	return r
}

// installReport prints one line per check and reports whether any check
// that is not a warning failed.
func installReport(w io.Writer, who string, rs []harness.CheckResult) (failed bool) {
	for _, r := range rs {
		st := "PASS"
		switch {
		case r.OK:
		case r.WarnOnly:
			st = "WARN"
		default:
			st, failed = "FAIL", true
		}
		fmt.Fprintf(w, "%s %s %s %s\n", r.ID, who, st, r.Detail)
	}
	return failed
}

// installChecks runs every target's checks; with skip, a target with no
// CrossTalk setup at all is skipped instead of failing its checks.
func installChecks(ctx context.Context, w io.Writer, targets []installTarget, env harness.InstallEnv, skip bool) bool {
	fmt.Fprintln(w, "== checks")
	failed := false
	for _, t := range targets {
		if skip {
			if ok, err := t.in.SetUp(env); !ok && err == nil {
				fmt.Fprintf(w, "%s: not set up, skipped\n", t.name)
				continue
			}
		}
		if installReport(w, t.name, t.in.Checks(ctx, env)) {
			failed = true
		}
	}
	if installReport(w, "crosstalk", []harness.CheckResult{installPathCheck(env.Bin)}) {
		failed = true
	}
	return failed
}

// installPathFiles are the shell startup files the PATH step edits on goos.
// Both a login and a non-login shell must see the bin dir.
func installPathFiles(goos, home string) []string {
	switch goos {
	case "darwin":
		return []string{filepath.Join(home, ".zprofile"), filepath.Join(home, ".zshenv")}
	case "linux":
		return []string{filepath.Join(home, ".profile"), filepath.Join(home, ".bashrc")}
	}
	return nil
}

func installPathLine(binDir string) string {
	return `export PATH="` + binDir + `:$PATH" # crosstalk`
}

func installAddPath(files []string, line string) error {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s := string(b)
		if slices.Contains(strings.Split(s, "\n"), line) {
			continue
		}
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		if err := harness.ReplaceFile(f, []byte(s+line+"\n")); err != nil {
			return err
		}
	}
	return nil
}

func installRemovePath(files []string, line string) error {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		lines := strings.Split(string(b), "\n")
		kept := slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return l == line })
		if len(kept) == len(lines) {
			continue
		}
		rest := strings.Join(kept, "\n")
		if strings.TrimSpace(rest) == "" {
			if err := os.Remove(f); err != nil {
				return err
			}
			continue
		}
		if err := harness.ReplaceFile(f, []byte(rest)); err != nil {
			return err
		}
	}
	return nil
}

// installOnLoginPath reports that a login shell already has binDir on PATH.
func installOnLoginPath(binDir string) bool {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	out, err := exec.Command(sh, "-lc", `printf %s "$PATH"`).Output()
	return err == nil && slices.Contains(filepath.SplitList(string(out)), binDir)
}

func installPathStep(binDir string, files []string) harness.Step {
	line := installPathLine(binDir)
	return harness.Step{
		Title:  "PATH (optional, only for typing crosstalk send)",
		Change: "append " + line + " to " + strings.Join(files, " and "),
		Apply: func(context.Context) error {
			return installAddPath(files, line)
		},
	}
}
