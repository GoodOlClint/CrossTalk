package opencode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type fakeSession struct {
	ID       string
	ParentID string
	Title    string
	Dir      string
	Updated  time.Time
	Archived bool
	Hidden   bool
}

func (s fakeSession) doc() map[string]any {
	tm := map[string]any{"created": s.Updated.UnixMilli(), "updated": s.Updated.UnixMilli()}
	if s.Archived {
		tm["archived"] = s.Updated.UnixMilli()
	}
	d := map[string]any{"id": s.ID, "title": s.Title, "location": map[string]any{"directory": s.Dir}, "time": tm}
	if s.ParentID != "" {
		d["parentID"] = s.ParentID
	}
	return d
}

type service struct {
	mu        sync.Mutex
	sessions  []fakeSession
	active    map[string]bool
	listExtra map[string]any
	status    int
	delay     time.Duration
}

func (s *service) set(f func(*service)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *service) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	status, delay := s.status, s.delay
	sessions := append([]fakeSession(nil), s.sessions...)
	active := map[string]bool{}
	for k, v := range s.active {
		active[k] = v
	}
	extra := s.listExtra
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch p := r.URL.Path; {
	case p == "/api/session":
		var data []any
		for _, x := range sessions {
			if !x.Hidden {
				data = append(data, x.doc())
			}
		}
		out := map[string]any{"data": data}
		for k, v := range extra {
			out[k] = v
		}
		write(out)
	case p == "/api/session/active":
		data := map[string]any{}
		for id := range active {
			data[id] = map[string]any{"type": "running"}
		}
		write(map[string]any{"data": data})
	case strings.HasPrefix(p, "/api/session/"):
		id := strings.TrimPrefix(p, "/api/session/")
		for _, x := range sessions {
			if x.ID == id {
				write(map[string]any{"data": x.doc()})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func startService(t *testing.T, sessions ...fakeSession) (*service, *fake) {
	t.Helper()
	s := &service{sessions: sessions, active: map[string]bool{}}
	return s, newFake(t, s.handle)
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
