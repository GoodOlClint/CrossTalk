package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const queueWake = "queue-wake (experimental)"

func rowOf(t *testing.T, s *Server, id string) Row {
	t.Helper()
	rows, _ := s.Listing(context.Background())
	for _, r := range rows {
		if r.AgentID == id {
			return r
		}
	}
	t.Fatalf("no row %s in %v", id, rows)
	return Row{}
}

type modeHarness struct {
	harness.Harness
	mu    sync.Mutex
	modes map[string]string
}

func (m *modeHarness) DeliveryModes(context.Context) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.modes {
		out[k] = v
	}
	return out
}

func (m *modeHarness) set(ref, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modes = map[string]string{ref: mode}
}

type discoverCounter struct {
	Backend
	n atomic.Int32
}

func (c *discoverCounter) Discover(ctx context.Context) ([]Session, error) {
	c.n.Add(1)
	return c.Backend.Discover(ctx)
}

func modeEnv(t *testing.T) (*env, *modeHarness, *discoverCounter, string) {
	t.Helper()
	fb := NewFakeBackend()
	e := newEnv(t, 999999, fb)
	mh := &modeHarness{Harness: e.srv.harnesses[0]}
	e.srv.harnesses[0] = mh
	dc := &discoverCounter{Backend: e.srv.backend}
	e.srv.backend = dc
	e.mustOK(thread("tab"), "list_agents", nil)
	return e, mh, dc, codexID("tab")
}

func peerShim(e *env) *Server {
	return New(Options{Backend: NewFakeBackend().AsBackend(), Registry: registry.New(e.stateDir), PPID: 1})
}

func TestDeliveryModeReachesAnotherShimAndClears(t *testing.T) {
	e, mh, _, id := modeEnv(t)
	other := peerShim(e)
	if got := rowOf(t, other, id).DeliveryMode; got != "" {
		t.Fatalf("mode before the serving shim ticked: %q", got)
	}
	mh.set("tab", queueWake)
	e.srv.syncDeliveryModes(context.Background())
	if got := rowOf(t, other, id).DeliveryMode; got != queueWake {
		t.Fatalf("second shim's row mode = %q, want %q", got, queueWake)
	}

	mh.set("tab", "")
	e.srv.syncDeliveryModes(context.Background())
	if got := rowOf(t, other, id).DeliveryMode; got != "" {
		t.Fatalf("mode after the claim was released = %q, want cleared", got)
	}
}

func TestSyncDeliveryModesRunsNoDiscover(t *testing.T) {
	e, mh, dc, _ := modeEnv(t)
	before := dc.n.Load()
	mh.set("tab", queueWake)
	e.srv.syncDeliveryModes(context.Background())
	if got := dc.n.Load(); got != before {
		t.Fatalf("sync ran Discover %d times", got-before)
	}
}

func TestDeliveryModeOfDeadOwnerIsNotShown(t *testing.T) {
	e, mh, _, id := modeEnv(t)
	mh.set("tab", queueWake)
	e.srv.syncDeliveryModes(context.Background())
	other := peerShim(e)
	if got := rowOf(t, other, id).DeliveryMode; got != queueWake {
		t.Fatalf("live owner's mode = %q, want %q", got, queueWake)
	}

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	path := e.reg.DescriptorPath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m["delivery_mode"] != queueWake {
		t.Fatalf("descriptor %v, %v", m, err)
	}
	m["pid"] = cmd.Process.Pid
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, _ := other.Listing(context.Background())
	for _, r := range rows {
		if r.AgentID == id {
			t.Fatalf("dead owner's row still listed with mode %q", r.DeliveryMode)
		}
	}
}

func TestOwnRowTrustsFreshDiscoverOverDescriptor(t *testing.T) {
	e, mh, _, id := modeEnv(t)
	mh.set("tab", queueWake)
	e.srv.syncDeliveryModes(context.Background())
	if got := rowOf(t, e.srv, id).DeliveryMode; got != "" {
		t.Fatalf("own row = %q, want the fresh Discover's default", got)
	}
}
