package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/harness/opencode"
	"github.com/GoodOlClint/crosstalk/internal/harness/pi"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/shim"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
	"github.com/GoodOlClint/crosstalk/internal/upgrade"
	"github.com/GoodOlClint/crosstalk/internal/wire"
)

const usage = `usage: crosstalk <command>

commands:
  install    install the binary and set up Claude Code and Codex
  uninstall  undo install
  reinstall  re-apply every install step
  mcp        run the shim as an MCP server over stdio
  remote     host identity and config for messaging other machines and pairing
  send       send a text to an agent by name, for plain scripts
  capture    record raw Codex app-server frames for fixtures
  hook       harness hook commands (codex session-start)
  version    print the version
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "mcp":
		return runMCP(stderr)
	case "install", "uninstall", "reinstall":
		return runInstall(args[0], args[1:], os.Stdin, isTerminal(os.Stdin), stdout, stderr)
	case "remote":
		return runRemote(args[1:], os.Stdin, isTerminal(os.Stdin), stdout, stderr)
	case "send":
		return runSend(args[1:], os.Stdin, stdout, stderr)
	case "capture":
		return runCapture(args[1:], stdout, stderr)
	case "hook":
		return runHook(args[1:], os.Stdin, stdout, stderr)
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case upgrade.SuperviseCmd:
		return upgrade.Supervise(args[1:], stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "crosstalk: unknown command %q\n%s", args[0], usage)
		return 2
	}
}

// runVersion prints the version; --json adds refresh_handoff, the handoff
// format this binary writes and reads, which a refreshing shim checks before
// it execs (ADR 0030).
func runVersion(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		fmt.Fprintln(stdout, mcpserver.Version)
	case len(args) == 1 && args[0] == "--json":
		b, _ := json.Marshal(struct {
			Version        string `json:"version"`
			RefreshHandoff int    `json:"refresh_handoff"`
			RefreshListens bool   `json:"refresh_listens"`
		}{mcpserver.Version, mcpserver.HandoffFormat, true})
		fmt.Fprintln(stdout, string(b))
	default:
		fmt.Fprintln(stderr, "usage: crosstalk version [--json]")
		return 2
	}
	return 0
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

// piHost decides whether the parent process is Pi; nil means pi.IsPiHost.
// Tests replace it because their parent is the test process.
var piHost func(pid int) bool

// harnesses is every supported harness in trust order: an explicit
// _meta.threadId outranks the parent pid.
func harnesses(stateDir string) ([]harness.Harness, error) {
	cl, err := claude.NewHarness(claude.HarnessOptions{Ingress: udsingress.NewIngress, Directory: registry.New(stateDir), StateDir: stateDir})
	if err != nil {
		return nil, err
	}
	ci := appserver.ClientInfo{Name: "crosstalk", Version: mcpserver.Version}
	cx := codex.NewHarness(codex.HarnessOptions{
		Adapter:    &codex.Adapter{Logger: slog.Default(), ClientInfo: ci, Endpoints: endpointList(os.Getenv("CROSSTALK_CODEX_ENDPOINTS"))},
		Logger:     slog.Default(),
		ClientInfo: ci,
		Directory:  registry.New(stateDir),
		Claims:     codex.ClaimsConfig{StateDir: stateDir},
	})
	oc := opencode.NewHarness(opencode.Options{Directory: registry.New(stateDir), Logger: slog.Default()})
	ph := pi.NewHarness(pi.Options{Directory: registry.New(stateDir), Logger: slog.Default(), Host: piHost})
	return []harness.Harness{cx, oc, ph, cl}, nil
}

// endpointList splits a comma-separated list; ws:// endpoints contain colons.
func endpointList(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func runMCP(stderr io.Writer) int {
	stateDir, err := registry.DefaultDir()
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk mcp: %v\n", err)
		return 1
	}
	hs, err := harnesses(stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk mcp: %v\n", err)
		return 1
	}
	w, err := wire.New(shim.Options{StateDir: stateDir, Harnesses: hs})
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk mcp: %v\n", err)
		return 1
	}
	sh := w.Shim()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := mcpserver.New(sh.MCPOptions())
	var runOpts []mcpserver.RunOption
	ho, err := mcpserver.HandoffFromEnv()
	if err != nil {
		slog.Warn("crosstalk mcp: refresh handoff unusable; starting normally", "err", err)
		ho = nil
	} else if ho != nil {
		srv.Resume(ctx, ho)
		runOpts = append(runOpts, mcpserver.WithHandoff(ho))
	}
	home, _ := os.UserHomeDir()
	in, arm, armed := startRefresh(ctx, home, hs, ho)
	tr := mcpserver.NewTransport(in, os.Stdout)
	arm(srv, srv, tr.Conn())
	stopFollowUp, _ := startFollowUp(ctx, armed, srv.Bound(), followUpEnv(os.Args[0], home, stateDir), hs, filepath.Join(home, ".config", "crosstalk"), srv.HoldNotice)
	stopClaims := serveClaims(ctx, hs, srv)
	err = serve(ctx, srv, tr, runOpts, stopFollowUp)
	return finish(srv, err, stopClaims, sh.Close, stderr)
}

// serve runs the server and, once it returns, cancels and waits for a running
// follow-up attempt so the shim does not exit with a plugin command half done.
func serve(ctx context.Context, srv *mcpserver.Server, tr *mcpserver.Transport, opts []mcpserver.RunOption, stopFollowUp func()) error {
	err := srv.Run(ctx, tr, opts...)
	stopFollowUp()
	return err
}

// finish stops the claim sources, closes the shim and reports the error; srv
// owns the lock files of timed-out agents until the process exits.
func finish(srv *mcpserver.Server, runErr error, stopClaims func(), closeShim func() error, stderr io.Writer) int {
	defer runtime.KeepAlive(srv)
	stopClaims()
	err := errors.Join(runErr, closeShim())
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "crosstalk mcp: %v\n", err)
		return 1
	}
	return 0
}

// serveClaims starts every harness's claim source; the returned func stops
// them and waits.
func serveClaims(ctx context.Context, hs []harness.Harness, r harness.SessionRegistrar) func() {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, h := range hs {
		cs, ok := h.(harness.ClaimSource)
		if !ok {
			continue
		}
		wg.Go(func() {
			if err := cs.ServeClaims(ctx, r); err != nil {
				slog.Warn("crosstalk mcp: claim source stopped", "harness", h.Name(), "err", err)
			}
		})
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 2 && args[0] == "codex" && args[1] == "session-start" {
		return codex.SessionStartHook(stdin, stdout, codex.HookOptions{})
	}
	fmt.Fprintf(stderr, "crosstalk hook: unknown hook %q\nusage: crosstalk hook codex session-start\n", strings.Join(args, " "))
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
