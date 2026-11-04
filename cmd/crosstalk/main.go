package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
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
  mcp      run the shim as an MCP server over stdio
  capture  record raw Codex app-server frames for fixtures
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "mcp":
		return runMCP(stderr)
	case "capture":
		return runCapture(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "crosstalk: unknown command %q\n%s", args[0], usage)
		return 2
	}
}

// harnesses is every supported harness in trust order: an explicit
// _meta.threadId outranks the parent pid.
func harnesses(stateDir string) ([]harness.Harness, error) {
	cl, err := claude.NewHarness(claude.HarnessOptions{Ingress: udsingress.NewIngress})
	if err != nil {
		return nil, err
	}
	cx := codex.NewHarness(codex.HarnessOptions{
		Logger:     slog.Default(),
		ClientInfo: appserver.ClientInfo{Name: "crosstalk", Version: mcpserver.Version},
		Directory:  registry.New(stateDir),
	})
	return []harness.Harness{cx, cl}, nil
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
	err = errors.Join(mcpserver.New(sh.MCPOptions()).Run(ctx, &mcp.StdioTransport{}), sh.Close())
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "crosstalk mcp: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
