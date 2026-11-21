package udsingress

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestListen_PathTooLong_ExpectErrPathTooLong(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", MaxSockPath), "a.sock")
	err := (&Server{Path: long}).Listen()
	if !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("Listen on %d-byte path = %v, want ErrPathTooLong", len(long), err)
	}
}
