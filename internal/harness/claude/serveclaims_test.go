package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// fakeDir is the descriptor directory; fakeReg publishes into it under
// regName, as mcpserver does under a fallback name when the harness name is
// held.
type fakeDir struct {
	mu      sync.Mutex
	ds      []registry.Descriptor
	regName string
	calls   map[string]int
}

func (f *fakeDir) List() ([]registry.Descriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]registry.Descriptor(nil), f.ds...), nil
}

func (f *fakeDir) set(ds ...registry.Descriptor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ds = ds
}

func (f *fakeDir) setRegName(n string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regName = n
}

func (f *fakeDir) count(sid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[sid]
}

type fakeReg struct {
	dir      *fakeDir
	sessions string
	ppid     int
}

func (r *fakeReg) RegisterSession(_ context.Context, req harness.RequestContext, _ string) error {
	b, err := os.ReadFile(filepath.Join(r.sessions, strconv.Itoa(req.ParentPID)+".json"))
	if err != nil {
		return err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	r.dir.mu.Lock()
	defer r.dir.mu.Unlock()
	r.dir.calls[s.SessionID]++
	r.dir.ds = []registry.Descriptor{{AgentID: identity.AgentID(string(adapter.Claude), s.SessionID), Name: r.dir.regName}}
	return nil
}

func (r *fakeReg) RetireSession(context.Context, harness.RequestContext) (bool, error) {
	return false, nil
}

func writeClaimSession(t *testing.T, dir string, pid int, sid, name string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": sid, "name": name, "cwd": "/w"})
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func serveClaims(t *testing.T, sid, name string) (*fakeDir, string) {
	t.Helper()
	sessions := t.TempDir()
	writeClaimSession(t, sessions, 4100, sid, name)
	dir := &fakeDir{regName: name, calls: map[string]int{}}
	h, err := NewHarness(HarnessOptions{SessionsDir: sessions, PPID: 4100, PollInterval: time.Millisecond, Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeClaims(ctx, &fakeReg{dir: dir, sessions: sessions})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return dir, sessions
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A session the poll registered, then retired while the poll looked away
// (/clear to another session and /resume back), is registered again with the
// session file unchanged.
func TestServeClaimsReregistersRetiredSession(t *testing.T) {
	dir, _ := serveClaims(t, "sess-a", "bob")
	eventually(t, "first registration", func() bool { return dir.count("sess-a") == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := dir.count("sess-a"); n != 1 {
		t.Fatalf("%d registrations of a held session, want 1", n)
	}
	dir.set()
	eventually(t, "registration after retirement", func() bool { return dir.count("sess-a") == 2 })
}

// A registration under a fallback name is retried until the harness name is
// free, with no tool call.
func TestServeClaimsRetriesFallbackNameUntilFree(t *testing.T) {
	dir, _ := serveClaims(t, "sess-a", "bob")
	dir.setRegName("bob-1234")
	eventually(t, "retries under the fallback name", func() bool { return dir.count("sess-a") >= 3 })
	dir.setRegName("bob")
	eventually(t, "harness name taken", func() bool {
		ds, _ := dir.List()
		return len(ds) == 1 && ds[0].Name == "bob"
	})
	n := dir.count("sess-a")
	time.Sleep(20 * time.Millisecond)
	if m := dir.count("sess-a"); m != n {
		t.Fatalf("still registering once held under the harness name: %d -> %d", n, m)
	}
}

func TestServeClaimsSkipsSessionHeldElsewhere(t *testing.T) {
	sessions := t.TempDir()
	writeClaimSession(t, sessions, 4100, "sess-a", "bob")
	dir := &fakeDir{regName: "bob", calls: map[string]int{}}
	dir.set(registry.Descriptor{AgentID: identity.AgentID(string(adapter.Claude), "sess-a"), Name: "bob"})
	h, err := NewHarness(HarnessOptions{SessionsDir: sessions, PPID: 4100, PollInterval: time.Millisecond, Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	h.ServeClaims(ctx, &fakeReg{dir: dir, sessions: sessions})
	if n := dir.count("sess-a"); n != 0 {
		t.Fatalf("%d registrations of a session another shim holds", n)
	}
}
