package opencode

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
)

var t0 = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func req(meta map[string]any) harness.RequestContext {
	return harness.RequestContext{Meta: meta}
}

func call(id any) harness.RequestContext {
	return req(map[string]any{MetaSessionID: id})
}

type rig struct {
	h    *Harness
	svc  *service
	f    *fake
	path string
	clk  *clock
}

func newRig(t *testing.T, version string, dir descriptors, sessions ...fakeSession) *rig {
	t.Helper()
	svc, f := startService(t, sessions...)
	path := writeSvc(t, t.TempDir(), f.srv.URL, version, 4242)
	clk := &clock{t: t0}
	h := NewHarness(Options{ServicePath: path, Directory: dir, Now: clk.now})
	return &rig{h: h, svc: svc, f: f, path: path, clk: clk}
}

func (r *rig) count(path string) int {
	n := 0
	for _, q := range r.f.requests() {
		if q.Path == path {
			n++
		}
	}
	return n
}

func noTUI(t *testing.T, f *fake) {
	t.Helper()
	for _, q := range f.requests() {
		if strings.Contains(q.Path, "/tui/") {
			t.Errorf("request to a /tui/ path: %s %s", q.Method, q.Path)
		}
	}
}

func clean(t *testing.T, what string, v any) {
	t.Helper()
	for _, f := range []string{"%v", "%+v", "%#v"} {
		noPassword(t, what+" "+f, fmt.Sprintf(f, v))
	}
}

func TestName(t *testing.T) {
	if got := NewHarness(Options{}).Name(); got != adapter.OpenCode {
		t.Fatalf("Name() = %q", got)
	}
}

func TestIdentify(t *testing.T) {
	r := newRig(t, "2.4.0", nil)
	cases := []struct {
		name string
		req  harness.RequestContext
		ok   bool
	}{
		{"valid id", call("ses_abc123"), true},
		{"no _meta", harness.RequestContext{}, false},
		{"other _meta only", req(map[string]any{"threadId": "ses_abc123"}), false},
		{"malformed id", call("ses_abc-123"), false},
		{"wrong prefix", call("sess_abc123"), false},
		{"empty id", call(""), false},
		{"bare prefix", call("ses_"), false},
		{"non-string id", call(12345), false},
		{"unbound", call("unbound"), false},
	}
	for _, tc := range cases {
		s, ok, err := r.h.Identify(context.Background(), tc.req)
		if err != nil {
			t.Errorf("%s: err %v", tc.name, err)
		}
		if ok != tc.ok {
			t.Errorf("%s: ok=%v want %v", tc.name, ok, tc.ok)
			continue
		}
		if !ok {
			if s != (harness.Session{}) {
				t.Errorf("%s: session %+v for an unclaimed call", tc.name, s)
			}
			continue
		}
		want := harness.Session{Harness: adapter.OpenCode, SessionRef: "ses_abc123", HarnessPID: 4242}
		if s != want {
			t.Errorf("%s: %+v want %+v", tc.name, s, want)
		}
		clean(t, tc.name, s)
	}
	if n := len(r.f.requests()); n != 0 {
		t.Errorf("Identify contacted the service %d times", n)
	}
}

func TestPooledSessionsNeverCrossBind(t *testing.T) {
	a := fakeSession{ID: "ses_aaa", Title: "A", Updated: t0}
	b := fakeSession{ID: "ses_bbb", Title: "B", Updated: t0}
	r := newRig(t, "2.4.0", nil, a, b)
	ctx := context.Background()
	for _, id := range []string{"ses_aaa", "ses_bbb", "ses_aaa", "ses_bbb"} {
		s, ok, err := r.h.Identify(ctx, call(id))
		if err != nil || !ok || s.SessionRef != id {
			t.Fatalf("Identify %s: %+v %v %v", id, s, ok, err)
		}
		bd, ok, err := r.h.Bind(ctx, call(id))
		if err != nil || !ok || bd.SessionRef != id {
			t.Fatalf("Bind %s: %+v %v %v", id, bd, ok, err)
		}
	}
	var paths []string
	for _, q := range r.f.requests() {
		paths = append(paths, q.Path)
	}
	want := []string{"/api/session/ses_aaa", "/api/session/ses_bbb", "/api/session/ses_aaa", "/api/session/ses_bbb"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("requests %v want %v", paths, want)
	}
}

func TestBind(t *testing.T) {
	r := newRig(t, "2.4.0", nil, fakeSession{ID: sid, Updated: t0})
	b, ok, err := r.h.Bind(context.Background(), call(sid))
	if err != nil || !ok {
		t.Fatalf("Bind: %v %v", ok, err)
	}
	want := adapter.Binding{Harness: adapter.OpenCode, SessionRef: sid, HarnessEndpoint: r.f.srv.URL, HarnessVersion: "2.4.0", HarnessPID: 4242}
	if b != want {
		t.Fatalf("binding %+v want %+v", b, want)
	}
	clean(t, "binding", b)
	if b, ok, err := r.h.Bind(context.Background(), call(12)); ok || err != nil || b != (adapter.Binding{}) {
		t.Errorf("Bind of an unclaimed call: %+v %v %v", b, ok, err)
	}
}

func TestBindVersionGate(t *testing.T) {
	for _, tc := range []struct {
		version string
		ok      bool
	}{
		{"3.0.0", false},
		{"1.4.2", false},
		{"2.9.0", true},
		{"2.4.1", true},
		{"two", false},
		{"", false},
	} {
		r := newRig(t, tc.version, nil, fakeSession{ID: sid, Updated: t0})
		b, claimed, err := r.h.Bind(context.Background(), call(sid))
		if !claimed {
			t.Errorf("%q: not claimed", tc.version)
		}
		if (err == nil) != tc.ok {
			t.Errorf("%q: err %v, want ok=%v", tc.version, err, tc.ok)
		}
		if !tc.ok {
			if b != (adapter.Binding{}) {
				t.Errorf("%q: binding %+v on refusal", tc.version, b)
			}
			if r.count("/api/session/"+sid) != 0 {
				t.Errorf("%q: contacted the service on a refused version", tc.version)
			}
		}
		if err != nil {
			clean(t, "version error", err)
		}
	}
}

func TestBindFailsClosed(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "2.4.0", nil, fakeSession{ID: "ses_other", Updated: t0}, fakeSession{ID: "ses_gone", Updated: t0, Archived: true})
	for _, id := range []string{sid, "ses_gone"} {
		b, ok, err := r.h.Bind(ctx, call(id))
		if !ok || err == nil || b != (adapter.Binding{}) {
			t.Errorf("Bind of %s: %+v %v %v", id, b, ok, err)
		}
		if err != nil {
			clean(t, "missing session error", err)
		}
	}
	missing := NewHarness(Options{ServicePath: filepath.Join(t.TempDir(), "service.json")})
	if b, ok, err := missing.Bind(ctx, call(sid)); !ok || err == nil || b != (adapter.Binding{}) {
		t.Errorf("Bind with no service.json: %+v %v %v", b, ok, err)
	}
	unsafe := writeService(t, t.TempDir(), r.f.srv.URL, testPassword, 0o644)
	h := NewHarness(Options{ServicePath: unsafe})
	b, ok, err := h.Bind(ctx, call(sid))
	if !ok || err == nil || b != (adapter.Binding{}) {
		t.Errorf("Bind with an unsafe service.json: %+v %v %v", b, ok, err)
	}
	if err != nil {
		clean(t, "unsafe error", err)
	}
	if _, err := r.h.Register(ctx, sid); err == nil {
		t.Error("Register of a missing session succeeded")
	}
	if b, err := r.h.Register(ctx, "ses_other"); err != nil || b.SessionRef != "ses_other" || b.HarnessPID != 4242 {
		t.Errorf("Register: %+v %v", b, err)
	}
}

func refs(ss []harness.Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.SessionRef)
	}
	sort.Strings(out)
	return out
}

func TestDiscoverFilter(t *testing.T) {
	sessions := []fakeSession{
		{ID: "ses_live", Title: "Live", Dir: "/w", Updated: t0.Add(-time.Hour)},
		{ID: "ses_busy", Title: "Busy", Dir: "/w", Updated: t0.Add(-2 * time.Hour)},
		{ID: "ses_archived", Updated: t0.Add(-time.Hour), Archived: true},
		{ID: "ses_child", ParentID: "ses_live", Updated: t0.Add(-time.Hour)},
		{ID: "ses_old", Updated: t0.Add(-25 * time.Hour)},
		{ID: "ses_boundold", Title: "BoundOld", Dir: "/b", Updated: t0.Add(-72 * time.Hour)},
		{ID: "ses_boundhidden", Title: "Hidden", Updated: t0.Add(-72 * time.Hour), Hidden: true},
		{ID: "ses_boundarchived", Updated: t0.Add(-time.Hour), Archived: true},
	}
	dir := append(bound("ses_boundold", "ses_boundhidden", "ses_boundarchived", "ses_boundgone"),
		descriptors{{Harness: "codex", SessionRef: "ses_old"}}...)
	r := newRig(t, "2.4.0", dir, sessions...)
	r.svc.Set(func(s *service) { s.Active["ses_busy"] = true })
	got, err := r.h.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ses_boundhidden", "ses_boundold", "ses_busy", "ses_live"}
	if strings.Join(refs(got), " ") != strings.Join(want, " ") {
		t.Fatalf("Discover %v want %v", refs(got), want)
	}
	for _, s := range got {
		if s.Harness != adapter.OpenCode || !s.Reachable || s.HarnessPID != 4242 || s.Endpoint != r.f.srv.URL {
			t.Errorf("row %+v", s)
		}
		wantStatus := "idle"
		if s.SessionRef == "ses_busy" {
			wantStatus = "busy"
		}
		if s.Status != wantStatus {
			t.Errorf("%s status %q want %q", s.SessionRef, s.Status, wantStatus)
		}
		if s.SessionRef == "ses_live" && (s.Name != "Live" || s.Cwd != "/w") {
			t.Errorf("live row %+v", s)
		}
		if s.SessionRef == "ses_boundold" && (s.Name != "BoundOld" || s.Cwd != "/b") {
			t.Errorf("bound-old row %+v", s)
		}
		clean(t, "row", s)
	}
	noTUI(t, r.f)
}

func TestDiscoverCap(t *testing.T) {
	var sessions []fakeSession
	for i := 0; i < DiscoverLimit+1; i++ {
		sessions = append(sessions, fakeSession{ID: fmt.Sprintf("ses_n%02d", i), Updated: t0.Add(-time.Duration(i) * time.Minute)})
	}
	r := newRig(t, "2.4.0", nil, sessions...)
	got, err := r.h.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != DiscoverLimit {
		t.Fatalf("%d rows, want %d", len(got), DiscoverLimit)
	}
	for _, s := range got {
		if s.SessionRef == fmt.Sprintf("ses_n%02d", DiscoverLimit) {
			t.Errorf("the 31st newest session %s is listed", s.SessionRef)
		}
	}
	if DiscoverWindow != 24*time.Hour || DiscoverLimit != 30 {
		t.Errorf("D4 constants %v %d", DiscoverWindow, DiscoverLimit)
	}
}

func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	l.Close()
	return u
}

func TestDiscoverServiceDown(t *testing.T) {
	ctx := context.Background()
	missing := NewHarness(Options{ServicePath: filepath.Join(t.TempDir(), "service.json"), Directory: bound(sid)})
	if got, err := missing.Discover(ctx); err != nil || len(got) != 0 {
		t.Errorf("missing service.json: %v %v", got, err)
	}
	down := NewHarness(Options{ServicePath: writeSvc(t, t.TempDir(), closedURL(t), "2.4.0", 4242), Directory: bound(sid)})
	if got, err := down.Discover(ctx); err != nil || len(got) != 0 {
		t.Errorf("service down: %v %v", got, err)
	}
	r := newRig(t, "2.4.0", bound(sid), fakeSession{ID: sid, Updated: t0})
	r.svc.Set(func(s *service) { s.Status = 401 })
	if got, err := r.h.Discover(ctx); err != nil || len(got) != 0 {
		t.Errorf("401: %v %v", got, err)
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "2.4.0", nil,
		fakeSession{ID: "ses_idle", Updated: t0},
		fakeSession{ID: "ses_busy", Updated: t0},
		fakeSession{ID: "ses_arch", Updated: t0, Archived: true})
	r.svc.Set(func(s *service) { s.Active["ses_busy"] = true })
	for _, tc := range []struct {
		id   string
		want adapter.SessionStatus
	}{
		{"ses_idle", adapter.SessionStatus{Loaded: true, Reachable: true}},
		{"ses_busy", adapter.SessionStatus{Loaded: true, Busy: true, Reachable: true}},
		{"ses_arch", adapter.SessionStatus{Reachable: true}},
		{"ses_missing", adapter.SessionStatus{Reachable: true}},
	} {
		got, err := r.h.Status(ctx, tc.id)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v %v want %+v", tc.id, got, err, tc.want)
		}
	}
	missing := NewHarness(Options{ServicePath: filepath.Join(t.TempDir(), "service.json")})
	if got, _ := missing.Status(ctx, "ses_idle"); got != (adapter.SessionStatus{}) {
		t.Errorf("no service.json: %+v", got)
	}
	down := NewHarness(Options{ServicePath: writeSvc(t, t.TempDir(), closedURL(t), "2.4.0", 4242)})
	if got, _ := down.Status(ctx, "ses_idle"); got != (adapter.SessionStatus{}) {
		t.Errorf("service down: %+v", got)
	}
	r.svc.Set(func(s *service) { s.Status = 401 })
	got, err := r.h.Status(ctx, "ses_idle")
	if got.Reachable || got.Loaded {
		t.Errorf("401: %+v", got)
	}
	if err != nil {
		clean(t, "status error", err)
	}
	noTUI(t, r.f)
}

func sess(id string, pid int) harness.Session {
	return harness.Session{Harness: adapter.OpenCode, SessionRef: id, HarnessPID: pid}
}

func TestSupersededTrue(t *testing.T) {
	r := newRig(t, "2.4.0", nil,
		fakeSession{ID: "ses_live", Updated: t0},
		fakeSession{ID: "ses_arch", Updated: t0, Archived: true})
	if !r.h.Superseded(sess("ses_deleted", 4242)) {
		t.Error("404 session not superseded")
	}
	if !r.h.Superseded(sess("ses_arch", 4242)) {
		t.Error("archived session not superseded")
	}
	if r.h.Superseded(sess("ses_live", 4242)) {
		t.Error("live session superseded")
	}
	noTUI(t, r.f)
}

func TestSupersededFalseOnEveryError(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*service)
	}{
		{"401", func(s *service) { s.Status = 401 }},
		{"500", func(s *service) { s.Status = 500 }},
		{"503", func(s *service) { s.Status = 503 }},
		{"null data", func(s *service) { s.ListRaw = `{"data":null}` }},
		{"no data", func(s *service) { s.ListRaw = `{"sessions":[]}` }},
	}
	for _, tc := range cases {
		r := newRig(t, "2.4.0", nil, fakeSession{ID: "ses_arch", Updated: t0, Archived: true})
		r.svc.Set(tc.mod)
		if r.h.Superseded(sess("ses_deleted", 4242)) {
			t.Errorf("%s: superseded", tc.name)
		}
		if r.h.Superseded(sess("ses_arch", 4242)) {
			t.Errorf("%s: archived superseded on an error", tc.name)
		}
	}

	refused := NewHarness(Options{ServicePath: writeSvc(t, t.TempDir(), closedURL(t), "2.4.0", 4242)})
	if refused.Superseded(sess("ses_deleted", 4242)) {
		t.Error("refused connection: superseded")
	}
	missing := NewHarness(Options{ServicePath: filepath.Join(t.TempDir(), "service.json")})
	if missing.Superseded(sess("ses_deleted", 4242)) {
		t.Error("missing service.json: superseded")
	}

	r := newRig(t, "2.4.0", nil)
	if r.h.Superseded(sess("ses_deleted", 9999)) {
		t.Error("other-pid service: superseded")
	}
	if n := len(r.f.requests()); n != 0 {
		t.Errorf("other-pid service contacted %d times", n)
	}
	if r.h.Superseded(sess("ses_deleted", 0)) {
		t.Error("zero pid: superseded")
	}

	slow := newRig(t, "2.4.0", nil)
	slow.svc.Set(func(s *service) { s.Delay = 3 * time.Second })
	start := time.Now()
	if slow.h.Superseded(sess("ses_deleted", 4242)) {
		t.Error("slow service: superseded")
	}
	if el := time.Since(start); el > supersedeDeadline+time.Second {
		t.Errorf("slow service took %v, deadline %v", el, supersedeDeadline)
	}
}

func TestSupersededServiceReplaced(t *testing.T) {
	r := newRig(t, "2.4.0", nil)
	writeSvc(t, filepath.Dir(r.path), r.f.srv.URL, "2.4.0", 5151)
	setOwner(t, owners(5151))
	if r.h.Superseded(sess("ses_deleted", 4242)) {
		t.Error("descriptor of the old service pid superseded by the new service's 404")
	}
	if !r.h.Superseded(sess("ses_deleted", 5151)) {
		t.Error("404 from the descriptor's own service not superseded")
	}
}

func TestSupersededPagedList(t *testing.T) {
	r := newRig(t, "2.4.0", nil,
		fakeSession{ID: "ses_p1", Updated: t0},
		fakeSession{ID: "ses_p2", Updated: t0},
		fakeSession{ID: "ses_p3", Updated: t0},
		fakeSession{ID: "ses_p4", Updated: t0},
		fakeSession{ID: "ses_p5", Updated: t0, Archived: true})
	r.svc.Set(func(s *service) { s.PageSize = 2 })
	if r.h.Superseded(sess("ses_p4", 4242)) {
		t.Error("live session off the first page superseded")
	}
	if !r.h.Superseded(sess("ses_p5", 4242)) {
		t.Error("archived session off the first page not superseded")
	}
	if !r.h.Superseded(sess("ses_deleted", 4242)) {
		t.Error("deleted session not superseded")
	}
	if r.h.Superseded(sess("ses_p1", 4242)) {
		t.Error("live session on the first page superseded")
	}
	if n := r.count("/api/session"); n != 1 {
		t.Errorf("%d list requests, want 1", n)
	}
}

func TestSupersededCache(t *testing.T) {
	r := newRig(t, "2.4.0", nil,
		fakeSession{ID: "ses_a", Updated: t0},
		fakeSession{ID: "ses_b", Updated: t0},
		fakeSession{ID: "ses_c", Updated: t0, Archived: true})
	for _, id := range []string{"ses_a", "ses_b", "ses_c", "ses_a", "ses_c"} {
		r.h.Superseded(sess(id, 4242))
	}
	if n := r.count("/api/session"); n != 1 {
		t.Fatalf("%d list requests inside 5 s, want 1", n)
	}
	r.clk.add(supersedeTTL + time.Millisecond)
	r.h.Superseded(sess("ses_a", 4242))
	if n := r.count("/api/session"); n != 2 {
		t.Fatalf("%d list requests after the TTL, want 2", n)
	}
	if n := len(r.f.requests()); n != 4 {
		t.Errorf("%d requests in all, want 4: two lists and a GET for each of the two checks of the archived session", n)
	}
}

func TestIdentifyNeedsServiceFile(t *testing.T) {
	missing := NewHarness(Options{ServicePath: filepath.Join(t.TempDir(), "service.json")})
	s, ok, err := missing.Identify(context.Background(), call(sid))
	if !ok || err == nil || s != (harness.Session{}) {
		t.Fatalf("Identify with no service.json: %+v %v %v", s, ok, err)
	}
}

func TestSupersededMalformedArchived(t *testing.T) {
	for _, raw := range []string{`""`, `{}`, `"2026-11-23"`, `true`, `-5`, `0`, `[]`} {
		r := newRig(t, "2.4.0", nil, fakeSession{ID: sid, Updated: t0, RawArchived: raw})
		if r.h.Superseded(sess(sid, 4242)) {
			t.Errorf("archived=%s: superseded", raw)
		}
	}
}

func TestSupersededReconfirms(t *testing.T) {
	r := newRig(t, "2.4.0", nil, fakeSession{ID: sid, Updated: t0, ListArchived: true})
	if r.h.Superseded(sess(sid, 4242)) {
		t.Error("archived in a cached list but live on GET: superseded")
	}
}

func TestSupersededServiceReplacedMidCall(t *testing.T) {
	r := newRig(t, "2.4.0", nil)
	r.svc.Set(func(s *service) {
		s.OnList = func() { writeSvc(t, filepath.Dir(r.path), r.f.srv.URL, "2.4.0", 5151) }
	})
	if r.h.Superseded(sess("ses_deleted", 4242)) {
		t.Error("404 from a service that replaced the descriptor's one mid-call: superseded")
	}
	for _, q := range r.f.requests() {
		if q.Path == "/api/session/ses_deleted" {
			t.Error("GET sent to the replacement service")
		}
	}
}

func TestSupersededSharedBudget(t *testing.T) {
	r := newRig(t, "2.4.0", nil)
	r.svc.Set(func(s *service) { s.GetDelay = time.Second })
	start := time.Now()
	for i := 0; i < 6; i++ {
		if r.h.Superseded(sess(fmt.Sprintf("ses_gone%d", i), 4242)) && i > 0 {
			t.Errorf("ses_gone%d superseded after the window's budget was spent", i)
		}
	}
	if el := time.Since(start); el > supersedeDeadline+time.Second {
		t.Errorf("six absent sessions took %v; one window's budget is %v", el, supersedeDeadline)
	}
}

func TestUntestedMajorIsNoEvidence(t *testing.T) {
	r := newRig(t, "3.0.0", bound(sid), fakeSession{ID: sid, Updated: t0, Archived: true}, fakeSession{ID: "ses_live", Updated: t0})
	if r.h.Superseded(sess(sid, 4242)) || r.h.Superseded(sess("ses_deleted", 4242)) {
		t.Error("major 3 service retired a session")
	}
	if got, err := r.h.Discover(context.Background()); err != nil || len(got) != 0 {
		t.Errorf("major 3 Discover: %v %v", got, err)
	}
	if n := len(r.f.requests()); n != 0 {
		t.Errorf("major 3 service contacted %d times", n)
	}
}

func TestActiveUnknownShape(t *testing.T) {
	r := newRig(t, "2.4.0", nil, fakeSession{ID: sid, Updated: t0})
	r.svc.Set(func(s *service) { s.ActiveRaw = `{}` })
	got, err := r.h.Discover(context.Background())
	if err != nil || len(got) != 1 || got[0].Status != "" {
		t.Errorf("unknown activity shape reported a status: %+v %v", got, err)
	}
}

func TestSupersededKeepsOneSnapshot(t *testing.T) {
	r := newRig(t, "2.4.0", nil, fakeSession{ID: sid, Updated: t0})
	r.h.Superseded(sess(sid, 4242))
	writeSvc(t, filepath.Dir(r.path), r.f.srv.URL, "2.4.0", 5151)
	r.h.Superseded(sess(sid, 5151))
	if r.h.list.pid != 5151 {
		t.Errorf("snapshot pid %d, want the current service's", r.h.list.pid)
	}
}

func TestSupersededUnarchiveWithinTTL(t *testing.T) {
	r := newRig(t, "2.4.0", nil, fakeSession{ID: sid, Updated: t0, Archived: true})
	if !r.h.Superseded(sess(sid, 4242)) {
		t.Fatal("archived session not superseded")
	}
	r.svc.Set(func(s *service) { s.Sessions[0].Archived = false })
	r.clk.add(time.Second)
	if r.h.Superseded(sess(sid, 4242)) {
		t.Error("unarchived within the TTL: still superseded")
	}
}

func TestSupersededBare404IsNotDeleted(t *testing.T) {
	r := newRig(t, "2.4.0", nil)
	r.svc.Set(func(s *service) { s.BareNotFound = true })
	if r.h.Superseded(sess("ses_deleted", 4242)) {
		t.Error("404 without SessionNotFoundError: superseded")
	}
	st, err := r.h.Status(context.Background(), "ses_deleted")
	if err != nil || st.Reachable || st.Loaded {
		t.Errorf("status on a bare 404: %+v %v", st, err)
	}
}
