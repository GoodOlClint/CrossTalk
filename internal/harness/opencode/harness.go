package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const MetaSessionID = "ai.opencode/sessionID"

// D4 filter (ADR 0034).
const (
	DiscoverWindow = 24 * time.Hour
	DiscoverLimit  = 30
)

// D7: one list per service pid per supersedeTTL; the registry sweep runs
// Superseded synchronously, so the whole call is bounded by supersedeDeadline.
const (
	supersedeTTL      = 5 * time.Second
	supersedeDeadline = 1500 * time.Millisecond
)

type Options struct {
	// ServicePath "" means DefaultServicePath.
	ServicePath string
	// Directory supplies the descriptors whose OpenCode sessions Discover
	// lists regardless of the D4 filter; nil means none.
	Directory interface {
		List() ([]registry.Descriptor, error)
	}
	Now    func() time.Time
	Logger *slog.Logger
}

type Harness struct {
	client *Client
	dir    interface {
		List() ([]registry.Descriptor, error)
	}
	now    func() time.Time
	logger *slog.Logger

	mu    sync.Mutex
	lists map[int]sessionList
}

var _ harness.Supersession = (*Harness)(nil)

func NewHarness(opt Options) *Harness {
	h := &Harness{client: NewClient(opt.ServicePath), dir: opt.Directory, now: opt.Now, logger: opt.Logger, lists: map[int]sessionList{}}
	if h.now == nil {
		h.now = time.Now
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	return h
}

func (h *Harness) Name() adapter.Harness { return adapter.OpenCode }

type sessionDoc struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID"`
	Title    string `json:"title"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Time struct {
		Updated  float64         `json:"updated"`
		Archived json.RawMessage `json:"archived"`
	} `json:"time"`
}

func (d sessionDoc) archived() bool {
	a := strings.TrimSpace(string(d.Time.Archived))
	return a != "" && a != "null" && a != "0" && a != "false"
}

func (d sessionDoc) updated() time.Time { return time.UnixMilli(int64(d.Time.Updated)) }

func sessionRef(req harness.RequestContext) (string, bool) {
	id, ok := req.Meta[MetaSessionID].(string)
	return id, ok && sessionID.MatchString(id)
}

func (h *Harness) service() (Service, *Client, error) {
	svc, err := ReadService(h.client.ServicePath)
	if err != nil {
		return Service{}, nil, err
	}
	c := *h.client
	c.PinOrigin = svc.Origin
	return svc, &c, nil
}

// Identify claims a call whose _meta carries an OpenCode session id. It reads
// service.json for the service pid and contacts nothing.
func (h *Harness) Identify(_ context.Context, req harness.RequestContext) (harness.Session, bool, error) {
	id, ok := sessionRef(req)
	if !ok {
		return harness.Session{}, false, nil
	}
	s := harness.Session{Harness: adapter.OpenCode, SessionRef: id}
	if svc, err := ReadService(h.client.ServicePath); err == nil {
		s.HarnessPID = svc.PID
	}
	return s, true, nil
}

func (h *Harness) Bind(ctx context.Context, req harness.RequestContext) (adapter.Binding, bool, error) {
	id, ok := sessionRef(req)
	if !ok {
		return adapter.Binding{}, false, nil
	}
	b, err := h.Register(ctx, id)
	return b, true, err
}

// Register binds a session the service has and has not archived, on a
// service whose major is tested.
func (h *Harness) Register(ctx context.Context, id string) (adapter.Binding, error) {
	svc, c, err := h.service()
	if err != nil {
		return adapter.Binding{}, fmt.Errorf("opencode: bind %s: %w", id, err)
	}
	warn, err := CheckVersion(svc.Version)
	if err != nil {
		return adapter.Binding{}, fmt.Errorf("opencode: bind %s: %w", id, err)
	}
	if warn != "" {
		h.logger.Warn("opencode: " + warn)
	}
	d, err := getSession(ctx, c, id)
	if err != nil {
		return adapter.Binding{}, fmt.Errorf("opencode: bind %s: %w", id, err)
	}
	if d.archived() {
		return adapter.Binding{}, fmt.Errorf("opencode: bind %s: session is archived", id)
	}
	return adapter.Binding{Harness: adapter.OpenCode, SessionRef: id, HarnessEndpoint: svc.Origin, HarnessVersion: svc.Version, HarnessPID: svc.PID}, nil
}

// Discover lists the D4 sessions plus every session a descriptor binds. A
// service that cannot be read or reached yields no rows.
func (h *Harness) Discover(ctx context.Context) ([]harness.Session, error) {
	svc, c, err := h.service()
	if err != nil {
		return nil, nil
	}
	docs, _, err := listSessions(ctx, c)
	if err != nil {
		return nil, nil
	}
	busy, activeErr := activeSessions(ctx, c)
	cutoff := h.now().Add(-DiscoverWindow)
	var pick []sessionDoc
	byID := map[string]sessionDoc{}
	for _, d := range docs {
		byID[d.ID] = d
		if !d.archived() && d.ParentID == "" && !d.updated().Before(cutoff) {
			pick = append(pick, d)
		}
	}
	sort.SliceStable(pick, func(i, j int) bool { return pick[i].Time.Updated > pick[j].Time.Updated })
	if len(pick) > DiscoverLimit {
		pick = pick[:DiscoverLimit]
	}
	seen := map[string]bool{}
	for _, d := range pick {
		seen[d.ID] = true
	}
	for _, ref := range h.boundRefs() {
		if seen[ref] {
			continue
		}
		d, ok := byID[ref]
		if !ok {
			if d, err = getSession(ctx, c, ref); err != nil {
				continue
			}
		}
		if !d.archived() {
			pick = append(pick, d)
			seen[ref] = true
		}
	}
	out := make([]harness.Session, 0, len(pick))
	for _, d := range pick {
		status := ""
		if activeErr == nil {
			status = "idle"
			if busy[d.ID] {
				status = "busy"
			}
		}
		out = append(out, harness.Session{Harness: adapter.OpenCode, SessionRef: d.ID, Name: d.Title, Cwd: d.Location.Directory,
			Status: status, Reachable: true, HarnessPID: svc.PID, Endpoint: svc.Origin})
	}
	return out, nil
}

func (h *Harness) boundRefs() []string {
	if h.dir == nil {
		return nil
	}
	ds, err := h.dir.List()
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range ds {
		if d.Harness == string(adapter.OpenCode) && sessionID.MatchString(d.SessionRef) {
			out = append(out, d.SessionRef)
		}
	}
	return out
}

func (h *Harness) Status(ctx context.Context, id string) (adapter.SessionStatus, error) {
	_, c, err := h.service()
	if err != nil {
		return adapter.SessionStatus{}, nil
	}
	d, err := getSession(ctx, c, id)
	var e *Error
	switch {
	case errors.As(err, &e) && e.Kind == KindNotFound:
		return adapter.SessionStatus{Reachable: true}, nil
	case errors.As(err, &e) && e.Kind == KindUnreachable:
		return adapter.SessionStatus{}, nil
	case err != nil:
		return adapter.SessionStatus{}, err
	case d.archived():
		return adapter.SessionStatus{Reachable: true}, nil
	}
	st := adapter.SessionStatus{Loaded: true, Reachable: true}
	if busy, err := activeSessions(ctx, c); err == nil {
		st.Busy = busy[id]
	}
	return st, nil
}

// Superseded is true only when the descriptor's own service (same pid)
// reports the session archived, or omits it from a complete list and answers
// 404 for it (D7). Anything else, including any error, is false.
func (h *Harness) Superseded(s harness.Session) bool {
	if s.HarnessPID <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), supersedeDeadline)
	defer cancel()
	svc, c, err := h.service()
	if err != nil || svc.PID != s.HarnessPID {
		return false
	}
	l := h.cachedList(ctx, svc.PID, c)
	if !l.ok {
		return false
	}
	if d, ok := l.sessions[s.SessionRef]; ok {
		return d.archived()
	}
	if !l.complete {
		return false
	}
	d, err := getSession(ctx, c, s.SessionRef)
	if err == nil {
		return d.archived()
	}
	var e *Error
	return errors.As(err, &e) && e.Kind == KindNotFound
}

type sessionList struct {
	at       time.Time
	ok       bool
	complete bool
	sessions map[string]sessionDoc
}

// ponytail: one lock across every pid's fetch; callers queue behind a slow
// list for at most supersedeDeadline. Per-pid locks if several services matter.
func (h *Harness) cachedList(ctx context.Context, pid int, c *Client) sessionList {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l, ok := h.lists[pid]; ok && h.now().Sub(l.at) < supersedeTTL {
		return l
	}
	l := sessionList{at: h.now()}
	if docs, complete, err := listSessions(ctx, c); err == nil {
		l.ok, l.complete, l.sessions = true, complete, make(map[string]sessionDoc, len(docs))
		for _, d := range docs {
			l.sessions[d.ID] = d
		}
	}
	h.lists[pid] = l
	return l
}

// listSessions reads GET /api/session. complete is false when the response
// carries any top-level field besides data, which may mark a further page.
func listSessions(ctx context.Context, c *Client) (docs []sessionDoc, complete bool, err error) {
	r, err := c.Sessions(ctx)
	if err != nil {
		return nil, false, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &top); err != nil {
		return nil, false, &Error{Kind: KindUnknown, Status: r.Status, Msg: "session list is not an object"}
	}
	if err := json.Unmarshal(top["data"], &docs); err != nil {
		return nil, false, &Error{Kind: KindUnknown, Status: r.Status, Msg: "session list has no data array"}
	}
	return docs, len(top) == 1, nil
}

func activeSessions(ctx context.Context, c *Client) (map[string]bool, error) {
	r, err := c.ActiveSessions(ctx)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &doc); err != nil {
		return nil, &Error{Kind: KindUnknown, Status: r.Status, Msg: "active list is not of the expected shape"}
	}
	out := make(map[string]bool, len(doc.Data))
	for id := range doc.Data {
		out[id] = true
	}
	return out, nil
}

func getSession(ctx context.Context, c *Client, id string) (sessionDoc, error) {
	r, err := c.Session(ctx, id)
	if err != nil {
		return sessionDoc{}, err
	}
	var doc struct {
		Data sessionDoc `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &doc); err != nil || doc.Data.ID != id {
		return sessionDoc{}, &Error{Kind: KindUnknown, Status: r.Status, Msg: "session response does not name the session"}
	}
	return doc.Data, nil
}
