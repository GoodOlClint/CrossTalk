package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/opencode/opencodetest"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type (
	fakeSession = opencodetest.Session
	service     = opencodetest.Service
	promptMode  = opencodetest.PromptMode
)

func startService(t *testing.T, sessions ...fakeSession) (*service, *fake) {
	t.Helper()
	s := opencodetest.NewService(sessions...)
	return s, newFake(t, s.Handle)
}

func writeSvc(t *testing.T, dir, url, version string, pid int) string {
	t.Helper()
	path := filepath.Join(dir, "service.json")
	body := fmt.Sprintf(`{"id":"svc_1","version":%q,"url":%q,"pid":%d,"password":%q}`, version, url, pid, testPassword)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type descriptors []registry.Descriptor

func (d descriptors) List() ([]registry.Descriptor, error) { return d, nil }

func bound(ids ...string) descriptors {
	var out descriptors
	for _, id := range ids {
		out = append(out, registry.Descriptor{Harness: "opencode", SessionRef: id})
	}
	return out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
