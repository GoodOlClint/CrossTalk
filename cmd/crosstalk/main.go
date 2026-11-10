package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/shim"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
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
		fmt.Fprintln(stdout, mcpserver.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "crosstalk: unknown command %q\n%s", args[0], usage)
		return 2
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

// harnesses is every supported harness in trust order: an explicit
// _meta.threadId outranks the parent pid.
func harnesses(stateDir string) ([]harness.Harness, error) {
	cl, err := claude.NewHarness(claude.HarnessOptions{Ingress: udsingress.NewIngress})
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
	return []harness.Harness{cx, cl}, nil
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
	stopClaims := serveClaims(ctx, hs, srv)
	err = srv.Run(ctx, &mcp.StdioTransport{})
	stopClaims()
	err = errors.Join(err, sh.Close())
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
