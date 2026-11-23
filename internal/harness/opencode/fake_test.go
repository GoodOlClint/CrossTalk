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
	// ListArchived marks the session archived in the list only, not in GET.
	ListArchived bool
	// RawArchived, when set, is the literal JSON of time.archived.
	RawArchived string
}

func (s fakeSession) doc(list bool) map[string]any {
	tm := map[string]any{"created": s.Updated.UnixMilli(), "updated": s.Updated.UnixMilli()}
	if s.Archived || list && s.ListArchived {
		tm["archived"] = s.Updated.UnixMilli()
	}
	if s.RawArchived != "" {
		tm["archived"] = json.RawMessage(s.RawArchived)
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
	getDelay  time.Duration
	listRaw   string
	activeRaw string
	onList    func()
	// prompt answers POST .../prompt; zero values give a 200 admitting the
	// message to the session in the path.
	prompt promptMode
}

type promptMode struct {
	Status  int
	Raw     string
	Session string
	Drop    bool
	Delay   time.Duration
	// RawStatus is the status sent with Raw; 0 means 200.
	RawStatus int
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
	extra, getDelay, listRaw, activeRaw, onList, pm := s.listExtra, s.getDelay, s.listRaw, s.activeRaw, s.onList, s.prompt
	s.mu.Unlock()
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt") {
		servePrompt(w, r, pm)
		return
	}
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
	case p == "/api/session" && listRaw != "":
		_, _ = w.Write([]byte(listRaw))
	case p == "/api/session":
		if onList != nil {
			onList()
		}
		data := []any{}
		for _, x := range sessions {
			if !x.Hidden {
				data = append(data, x.doc(true))
			}
		}
		out := map[string]any{"data": data}
		for k, v := range extra {
			out[k] = v
		}
		write(out)
	case p == "/api/session/active" && activeRaw != "":
		_, _ = w.Write([]byte(activeRaw))
	case p == "/api/session/active":
		data := map[string]any{}
		for id := range active {
			data[id] = map[string]any{"type": "running"}
		}
		write(map[string]any{"data": data})
	case strings.HasPrefix(p, "/api/session/"):
		select {
		case <-time.After(getDelay):
		case <-r.Context().Done():
			return
		}
		id := strings.TrimPrefix(p, "/api/session/")
		for _, x := range sessions {
			if x.ID == id {
				write(map[string]any{"data": x.doc(false)})
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

func servePrompt(w http.ResponseWriter, r *http.Request, pm promptMode) {
	select {
	case <-time.After(pm.Delay):
	case <-r.Context().Done():
		return
	}
	switch {
	case pm.Drop:
		if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = c.Close()
		}
		return
	case pm.Status != 0:
		w.WriteHeader(pm.Status)
		return
	case pm.Raw != "":
		w.Header().Set("Content-Type", "application/json")
		if pm.RawStatus != 0 {
			w.WriteHeader(pm.RawStatus)
		}
		_, _ = w.Write([]byte(pm.Raw))
		return
	}
	var body struct {
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if body.Delivery == "" {
		body.Delivery = "steer"
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/prompt")
	if pm.Session != "" {
		id = pm.Session
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
		"id": "msg_1", "sessionID": id, "type": "user", "payload": map[string]any{"text": body.Text}, "delivery": body.Delivery,
	}})
}
