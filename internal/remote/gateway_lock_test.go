package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestGatewayLockRetriesOnce(t *testing.T) {
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	fd := int(f.Fd())
	time.AfterFunc(10*time.Millisecond, func() { unix.Flock(fd, unix.LOCK_UN) })
	release, err := acquireGatewayLock(dir)
	if err != nil {
		t.Fatalf("a lock released within the retry window: %v", err)
	}
	release()
}

func TestGatewayLockHeldPastRetryNamesOtherGateway(t *testing.T) {
	dir := t.TempDir()
	holdLock(t, dir)
	start := time.Now()
	_, err := acquireGatewayLock(dir)
	if err == nil || !strings.Contains(err.Error(), "another gateway holds") {
		t.Fatalf("held lock: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("retry took %v, want one bounded wait", d)
	}
}
