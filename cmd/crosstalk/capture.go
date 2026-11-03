package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

func runCapture(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	def, _ := appserver.DefaultControlSocket()
	endpoint := fs.String("endpoint", def, "app-server endpoint: socket path, unix://<path> or ws://host:port")
	thread := fs.String("thread", "", "exact Codex thread id (required)")
	out := fs.String("out", "", "output directory for "+codex.CaptureFile+" (required)")
	steer := fs.String("steer-text", "", "also steer this text into the thread's real active turn (delivered if steerable)")
	listen := fs.Duration("listen", 2*time.Second, "record notifications for this long before closing")
	timeout := fs.Duration("timeout", 30*time.Second, "overall timeout, excluding -listen")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: crosstalk capture -thread <id> -out <dir> [-endpoint <ep>] [-steer-text <text>] [-listen 2s]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *thread == "" || *out == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout+*listen)
	defer cancel()
	path, err := codex.Capture(ctx, codex.CaptureOptions{Endpoint: *endpoint, ThreadID: *thread, OutDir: *out, SteerText: *steer, Listen: *listen})
	if path != "" {
		fmt.Fprintln(stdout, path)
	}
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk capture: %v\n", err)
		return 1
	}
	return 0
}
