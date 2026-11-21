package udsingress

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sockOfLen(t *testing.T, n int) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	pad := n - len(filepath.Join(d, "a.sock")) - 1
	if pad < 1 {
		t.Fatalf("temp dir %q too long for %d", d, n)
	}
	sub := filepath.Join(d, strings.Repeat("d", pad))
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "a.sock")
	if len(p) != n {
		t.Fatalf("len %d, want %d", len(p), n)
	}
	return p
}

func TestListen_LastValidLength_ExpectBinds(t *testing.T) {
	s := &Server{Path: sockOfLen(t, MaxSockPath)}
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen at %d bytes: %v", MaxSockPath, err)
	}
	s.Close()
}

func TestListen_FirstInvalidLength_ExpectErrPathTooLong(t *testing.T) {
	err := (&Server{Path: sockOfLen(t, MaxSockPath+1)}).Listen()
	if !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("Listen at %d bytes = %v, want ErrPathTooLong", MaxSockPath+1, err)
	}
}
