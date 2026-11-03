package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/shim"
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

func runMCP(stderr io.Writer) int {
	sh, err := shim.New(shim.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk mcp: %v\n", err)
		return 1
	}
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
