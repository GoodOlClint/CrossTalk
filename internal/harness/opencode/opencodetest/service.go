package opencodetest

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Session struct {
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

func (s Session) Doc(list bool) map[string]any {
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

type Service struct {
	mu        sync.Mutex
	Sessions  []Session
	Active    map[string]bool
	ListExtra map[string]any
	Status    int
	Delay     time.Duration
	GetDelay  time.Duration
	ListRaw   string
	ActiveRaw string
	OnList    func()
	// Prompt answers POST .../prompt; zero values give a 200 admitting the
	// message to the session in the path.
	Prompt PromptMode
}

type PromptMode struct {
	Status  int
	Raw     string
	Session string
	Drop    bool
	Delay   time.Duration
	// RawStatus is the status sent with Raw; 0 means 200.
	RawStatus int
}

func (s *Service) Set(f func(*Service)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	status, delay := s.Status, s.Delay
	sessions := append([]Session(nil), s.Sessions...)
	active := map[string]bool{}
	for k, v := range s.Active {
		active[k] = v
	}
	extra, getDelay, listRaw, activeRaw, onList, pm := s.ListExtra, s.GetDelay, s.ListRaw, s.ActiveRaw, s.OnList, s.Prompt
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
				data = append(data, x.Doc(true))
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
				write(map[string]any{"data": x.Doc(false)})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func servePrompt(w http.ResponseWriter, r *http.Request, pm PromptMode) {
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

func NewService(sessions ...Session) *Service {
	return &Service{Sessions: sessions, Active: map[string]bool{}}
}
