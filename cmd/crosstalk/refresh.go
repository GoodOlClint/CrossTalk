package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/quiesce"
	"github.com/GoodOlClint/crosstalk/internal/refresh"
)

// refreshArmed reports whether argv0 is a binary path a harness registration
// in home runs, with refresh not turned off (ADR 0030).
func refreshArmed(argv0, home string, hs []harness.Harness) bool {
	if refresh.Off() || home == "" {
		return false
	}
	return refresh.Trusted(argv0, func(bin string) bool {
		for _, h := range hs {
			in, ok := h.(harness.Installer)
			if !ok {
				continue
			}
			if reg, err := in.Registered(harness.InstallEnv{Bin: bin, Home: home}); err == nil && reg {
				return true
			}
		}
		return false
	})
}

// pollableStdin returns fd 0 in non-blocking mode, so a read waiting on it
// can be interrupted by a deadline without consuming a byte.
func pollableStdin() (*os.File, error) {
	if err := syscall.SetNonblock(0, true); err != nil {
		return nil, err
	}
	f := os.NewFile(0, "/dev/stdin")
	if err := f.SetReadDeadline(time.Time{}); err != nil {
		syscall.SetNonblock(0, false)
		return nil, err
	}
	return f, nil
}

type ingressPauser interface{ PauseIngress() (unpause func()) }

// quiesceForExec stops admitting peer work, stops the stdin reader at a frame
// boundary, and waits until every read request is answered and no counted
// work runs. The listeners close only after that wait, so agents bound by a
// call still running are paused too.
func quiesceForExec(ctx context.Context, sh ingressPauser, conn *mcpserver.Conn) (func(), error) {
	if !quiesce.Pause() {
		return nil, errors.New("refresh: a pause is already in progress")
	}
	var unpause []func()
	undo := func() {
		for _, f := range unpause {
			f()
		}
		conn.Unpause()
		quiesce.Unpause()
	}
	unpause = append(unpause, sh.PauseIngress())
	if err := conn.Pause(); err != nil {
		undo()
		return nil, err
	}
	for {
		if err := conn.WaitPaused(ctx); err != nil {
			undo()
			return nil, err
		}
		if err := quiesce.Wait(ctx); err != nil {
			undo()
			return nil, err
		}
		unpause = append(unpause, sh.PauseIngress())
		if quiesce.Busy() == 0 && conn.WaitPaused(expired) == nil {
			return undo, nil
		}
	}
}

// expired makes a wait check its condition once.
var expired = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// execHandoff writes the handoff to an inherited descriptor and execs path
// with the same argv; it returns only when exec fails.
func execHandoff(srv *mcpserver.Server, conn *mcpserver.Conn, path string, id mcpserver.BinaryIdentity) error {
	f, err := mcpserver.WriteHandoff(srv.Handoff(conn.State(), id))
	if err != nil {
		return err
	}
	defer f.Close()
	fd := int(f.Fd())
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		return err
	}
	env := []string{mcpserver.HandoffFDEnv + "=" + strconv.Itoa(fd)}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, mcpserver.HandoffFDEnv+"=") {
			env = append(env, e)
		}
	}
	syscall.SetNonblock(0, false)
	err = syscall.Exec(path, os.Args, env)
	syscall.SetNonblock(0, true)
	runtime.KeepAlive(f)
	return fmt.Errorf("exec %s: %w", path, err)
}

// startRefresh arms the watcher when argv0 is trusted and returns the input
// the transport reads; unarmed, it returns stdin unchanged.
func startRefresh(ctx context.Context, home string, hs []harness.Harness, ho *mcpserver.Handoff) (io.Reader, func(srv *mcpserver.Server, sh ingressPauser, conn *mcpserver.Conn)) {
	argv0 := os.Args[0]
	if !refreshArmed(argv0, home, hs) {
		return os.Stdin, func(*mcpserver.Server, ingressPauser, *mcpserver.Conn) {}
	}
	id, err := mcpserver.IdentityOf(argv0)
	if ho != nil && ho.Binary.Path == argv0 {
		id, err = ho.Binary, nil
	}
	if err != nil {
		slog.Warn("crosstalk mcp: refresh not armed", "path", argv0, "err", err)
		return os.Stdin, func(*mcpserver.Server, ingressPauser, *mcpserver.Conn) {}
	}
	in, err := pollableStdin()
	if err != nil {
		slog.Warn("crosstalk mcp: refresh not armed: stdin cannot be paused", "err", err)
		return os.Stdin, func(*mcpserver.Server, ingressPauser, *mcpserver.Conn) {}
	}
	return in, func(srv *mcpserver.Server, sh ingressPauser, conn *mcpserver.Conn) {
		w := &refresh.Watcher{
			Path:    argv0,
			ID:      id,
			Quiesce: func(ctx context.Context) (func(), error) { return quiesceForExec(ctx, sh, conn) },
			Exec:    func(id mcpserver.BinaryIdentity) error { return execHandoff(srv, conn, argv0, id) },
		}
		go w.Run(ctx)
	}
}
