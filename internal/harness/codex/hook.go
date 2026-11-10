package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const DefaultHookTimeout = 2 * time.Second

var errNoCodexAncestor = errors.New("codex hook: no codex ancestor")

type HookOptions struct {
	// StateDir "" means registry.DefaultDir().
	StateDir string
	// SockDir "" means DefaultSockDir().
	SockDir string
	Sys     System
	// Self is the hook's own pid; 0 means os.Getpid().
	Self    int
	Timeout time.Duration
	Now     func() time.Time
	Getenv  func(string) string
}

// SessionStartHook is `crosstalk hook codex session-start`. It never fails
// Codex: whatever happens it writes "{}" to stdout within the time bound and
// returns 0. It never opens the journal.
func SessionStartHook(stdin io.Reader, stdout io.Writer, opt HookOptions) int {
	if opt.Timeout <= 0 {
		opt.Timeout = DefaultHookTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sessionStart(ctx, stdin, opt)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	fmt.Fprintln(stdout, "{}")
	return 0
}

func sessionStart(ctx context.Context, stdin io.Reader, opt HookOptions) error {
	if opt.StateDir == "" {
		d, err := registry.DefaultDir()
		if err != nil {
			return err
		}
		opt.StateDir = d
	}
	if !ClaimsEnabled(opt.StateDir) {
		return nil
	}
	if opt.Sys == nil {
		opt.Sys = Kernel{}
	}
	if opt.SockDir == "" {
		opt.SockDir = DefaultSockDir()
	}
	if opt.Self == 0 {
		opt.Self = os.Getpid()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Getenv == nil {
		opt.Getenv = os.Getenv
	}
	var in struct {
		SessionID      string `json:"session_id"`
		Source         string `json:"source"`
		Cwd            string `json:"cwd"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return err
	}
	if !sessionIDRe.MatchString(in.SessionID) {
		return ErrBadSessionID
	}
	owner, err := codexAncestor(opt.Sys, opt.Self)
	if err != nil {
		return err
	}
	cl := Claim{SessionID: in.SessionID, Source: in.Source, Cwd: in.Cwd, TranscriptPath: in.TranscriptPath,
		OwnerPID: owner.PID, OwnerStartMicros: owner.Start.UnixMicro(), ThreadEnv: opt.Getenv("CODEX_THREAD_ID"), At: opt.Now().UTC()}
	line, err := json.Marshal(cl)
	if err != nil {
		return err
	}
	if err := writeClaimFile(claimFile(opt.StateDir, owner, cl.SessionID), line); err != nil {
		return err
	}
	return sendLive(ctx, ClaimSocketPath(opt.SockDir, owner), line)
}

// codexAncestor is the nearest ancestor of self whose executable is codex.
func codexAncestor(sys System, self int) (ownerproof.Proc, error) {
	chain, err := ownerproof.AncestryWith(sys.Info, self)
	if err != nil {
		return ownerproof.Proc{}, err
	}
	for _, p := range chain[1:] {
		exe, argv, err := sys.Args(p.PID)
		if err == nil && isCodex(exe, argv) {
			return p, nil
		}
	}
	return ownerproof.Proc{}, errNoCodexAncestor
}

func writeClaimFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claim-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// sendLive waits for the shim's answer so that this process, the sender the
// shim names by peer credential, is still alive while the shim walks its
// ancestry.
func sendLive(ctx context.Context, path string, line []byte) error {
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return err
	}
	_, err = bufio.NewReader(conn).ReadString('\n')
	return err
}
