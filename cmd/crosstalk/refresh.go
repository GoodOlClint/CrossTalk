package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
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

type commandLister interface {
	RegisteredCommands(env harness.InstallEnv) ([]string, error)
}

// refreshArmed reports whether argv0 is, exactly, the command a harness
// registration in home names, with refresh not turned off (ADR 0030). A
// relative or symlink-equivalent command does not count.
func refreshArmed(argv0, home string, hs []harness.Harness) bool {
	if refresh.Off() || home == "" {
		return false
	}
	return refresh.Trusted(argv0, func(bin string) bool {
		for _, h := range hs {
			cl, ok := h.(commandLister)
			if !ok {
				continue
			}
			cmds, _ := cl.RegisteredCommands(harness.InstallEnv{Bin: bin, Home: home})
			if slices.Contains(cmds, bin) {
				return true
			}
		}
		return false
	})
}

// stdinFlags are fd 0's file status flags before pollableStdin changed them.
var stdinFlags = -1

// pollableStdin returns a close-on-exec duplicate of fd 0 in non-blocking
// mode, so a read waiting on it can be interrupted by a deadline without
// consuming a byte. O_NONBLOCK is shared with fd 0; on failure the flags are
// restored and only the duplicate is closed.
func pollableStdin() (*os.File, error) {
	fl, err := unix.FcntlInt(0, unix.F_GETFL, 0)
	if err != nil {
		return nil, err
	}
	fd, err := unix.FcntlInt(0, unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "/dev/stdin")
	if err := f.SetReadDeadline(time.Time{}); err != nil {
		f.Close()
		unix.FcntlInt(0, unix.F_SETFL, fl)
		return nil, err
	}
	stdinFlags = fl
	return f, nil
}

type ingressPauser interface{ PauseIngress() (unpause func()) }

// quiesceForExec stops admitting peer work, stops the stdin reader at a frame
// boundary, and waits until every read request is answered and no counted
// work runs. The listeners close only after that wait, so agents bound by a
// call still running are paused too. Undo opens the gate before reopening
// the listeners and resumes stdin last: an agent whose listener cannot listen
// again is retired, which waits for its running calls, and those may need the
// gate, while a fresh call from stdin could outlast that wait.
func quiesceForExec(ctx context.Context, sh ingressPauser, conn *mcpserver.Conn) (func(), error) {
	if !quiesce.Pause() {
		return nil, errors.New("refresh: a pause is already in progress")
	}
	var unpause []func()
	undo := func() {
		quiesce.Unpause()
		for _, f := range unpause {
			f()
		}
		conn.Unpause()
	}
	unpause = append(unpause, sh.PauseIngress())
	if err := conn.Pause(); err != nil {
		undo()
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			undo()
			return nil, err
		}
		if err := conn.WaitPaused(ctx); err != nil {
			undo()
			return nil, err
		}
		if err := quiesce.Wait(ctx); err != nil {
			undo()
			return nil, err
		}
		unpause = append(unpause, sh.PauseIngress())
		if err := ctx.Err(); err != nil {
			undo()
			return nil, err
		}
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

// errChanged: the file at the trusted path changed again while the handoff
// was prepared; the next tick probes it.
var errChanged = errors.New("refresh: binary replaced again before exec")

// execHandoff writes the handoff to a descriptor the new image inherits,
// checks that path is still the probed file, and execs it with the same
// argv; it returns only when it did not exec. While close-on-exec is clear on
// the handoff descriptor, syscall.ForkLock is held, so no child forked
// meanwhile inherits it.
func execHandoff(srv *mcpserver.Server, conn *mcpserver.Conn, path string, id mcpserver.BinaryIdentity) error {
	f, err := mcpserver.WriteHandoff(srv.Handoff(conn.State(), id))
	if err != nil {
		return err
	}
	defer f.Close()
	if cur, err := mcpserver.IdentityOf(path); err != nil || cur.Dev != id.Dev || cur.Ino != id.Ino {
		return errChanged
	}
	fd := f.Fd()
	env := []string{mcpserver.HandoffFDEnv + "=" + strconv.Itoa(int(fd))}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, mcpserver.HandoffFDEnv+"=") {
			env = append(env, e)
		}
	}
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if _, err := unix.FcntlInt(fd, unix.F_SETFD, 0); err != nil {
		return err
	}
	defer unix.FcntlInt(fd, unix.F_SETFD, unix.FD_CLOEXEC)
	if stdinFlags >= 0 {
		unix.FcntlInt(0, unix.F_SETFL, stdinFlags)
	}
	err = syscall.Exec(path, os.Args, env)
	syscall.SetNonblock(0, true)
	runtime.KeepAlive(f)
	return fmt.Errorf("%w: exec %s: %w", refresh.ErrExecFailed, path, err)
}

// startRefresh arms the watcher when argv0 is trusted and returns the input
// the transport reads; unarmed, it returns stdin unchanged. armed reports
// that argv0 is trusted, even when the watcher could not start.
func startRefresh(ctx context.Context, home string, hs []harness.Harness, ho *mcpserver.Handoff) (in io.Reader, arm func(srv *mcpserver.Server, sh ingressPauser, conn *mcpserver.Conn), armed bool) {
	argv0 := os.Args[0]
	if !refreshArmed(argv0, home, hs) {
		return os.Stdin, func(*mcpserver.Server, ingressPauser, *mcpserver.Conn) {}, false
	}
	id, err := mcpserver.IdentityOf(argv0)
	if ho != nil && ho.Binary.Path == argv0 {
		id, err = ho.Binary, nil
	}
	if err != nil {
		slog.Warn("crosstalk mcp: refresh not armed", "path", argv0, "err", err)
		return os.Stdin, func(*mcpserver.Server, ingressPauser, *mcpserver.Conn) {}, true
	}
	stdin, err := pollableStdin()
	if err != nil {
		slog.Warn("crosstalk mcp: refresh not armed: stdin cannot be paused", "err", err)
		return os.Stdin, func(*mcpserver.Server, ingressPauser, *mcpserver.Conn) {}, true
	}
	return stdin, func(srv *mcpserver.Server, sh ingressPauser, conn *mcpserver.Conn) {
		w := &refresh.Watcher{
			Path:    argv0,
			ID:      id,
			Quiesce: func(ctx context.Context) (func(), error) { return quiesceForExec(ctx, sh, conn) },
			Exec:    func(id mcpserver.BinaryIdentity) error { return execHandoff(srv, conn, argv0, id) },
		}
		go w.Run(ctx)
	}, true
}
