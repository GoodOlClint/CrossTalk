package main

import (
	"fmt"
	"io"
	"os"
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
		fmt.Fprintf(stderr, "crosstalk %s: not implemented\n", args[0])
		return 1
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
