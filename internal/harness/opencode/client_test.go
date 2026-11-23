package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	Method, Path, Auth, Body string
}

type fake struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, basic(r), string(b)})
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func basic(r *http.Request) string {
	u, p, ok := r.BasicAuth()
	if !ok {
		return ""
	}
	return u + ":" + p
}

func (f *fake) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func jsonOK(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func clientFor(t *testing.T, f *fake) *Client {
	t.Helper()
	return NewClient(writeService(t, t.TempDir(), f.srv.URL, testPassword, 0o600))
}

func wantKind(t *testing.T, err error, k Kind) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if e.Kind != k {
		t.Fatalf("kind %v, want %v (%v)", e.Kind, k, err)
	}
	noPassword(t, "error", err.Error())
	return e
}

const sid = "ses_abc123"

func TestClientCalls(t *testing.T) {
	f := newFake(t, jsonOK(`{"ok":true}`))
	c := clientFor(t, f)
	ctx := context.Background()
	calls := []struct {
		name         string
		do           func() (*Response, error)
		method, path string
	}{
		{"list", func() (*Response, error) { return c.Sessions(ctx) }, "GET", "/api/session"},
		{"active", func() (*Response, error) { return c.ActiveSessions(ctx) }, "GET", "/api/session/active"},
		{"get", func() (*Response, error) { return c.Session(ctx, sid) }, "GET", "/api/session/" + sid},
		{"prompt", func() (*Response, error) { return c.Prompt(ctx, sid, []byte(`{"text":"hi"}`)) }, "POST", "/api/session/" + sid + "/prompt"},
	}
	for _, tc := range calls {
		r, err := tc.do()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if r.Status != 200 || string(r.Body) != `{"ok":true}` {
			t.Errorf("%s: %d %q", tc.name, r.Status, r.Body)
		}
	}
	reqs := f.requests()
	if len(reqs) != len(calls) {
		t.Fatalf("recorded %d requests, want %d", len(reqs), len(calls))
	}
	wantAuth := "opencode:" + testPassword
	for i, tc := range calls {
		rq := reqs[i]
		if rq.Method != tc.method || rq.Path != tc.path {
			t.Errorf("%s: sent %s %s", tc.name, rq.Method, rq.Path)
		}
		if rq.Auth != wantAuth {
			t.Errorf("%s: wrong credential sent", tc.name)
		}
		if strings.Contains(rq.Path, "/tui/") {
			t.Errorf("%s: /tui/ path %q", tc.name, rq.Path)
		}
	}
	if reqs[3].Body != `{"text":"hi"}` {
		t.Errorf("prompt body %q", reqs[3].Body)
	}
}

func TestClientRejectsBadSessionID(t *testing.T) {
	f := newFake(t, jsonOK(`{}`))
	c := clientFor(t, f)
	for _, id := range []string{"", "..", "ses_x/../../tui/append-prompt", "ses_a b", "abc", "ses_"} {
		if _, err := c.Session(context.Background(), id); err == nil {
			t.Errorf("accepted id %q", id)
		}
		if _, err := c.Prompt(context.Background(), id, []byte(`{}`)); err == nil {
			t.Errorf("prompt accepted id %q", id)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent for bad ids", n)
	}
}

func TestClientUnreachable(t *testing.T) {
	t.Run("401", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
		_, err := clientFor(t, f).Session(context.Background(), sid)
		e := wantKind(t, err, KindUnreachable)
		if e.Status != 401 {
			t.Errorf("status %d", e.Status)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		url := "http://" + l.Addr().String()
		l.Close()
		c := NewClient(writeService(t, t.TempDir(), url, testPassword, 0o600))
		_, err = c.Prompt(context.Background(), sid, []byte(`{}`))
		wantKind(t, err, KindUnreachable)
	})
	t.Run("missing service.json sends nothing", func(t *testing.T) {
		f := newFake(t, jsonOK(`{}`))
		c := NewClient(t.TempDir() + "/service.json")
		_, err := c.Sessions(context.Background())
		e := wantKind(t, err, KindUnreachable)
		var se *ServiceError
		if !errors.As(e, &se) {
			t.Errorf("service error not wrapped: %v", err)
		}
		if len(f.requests()) != 0 {
			t.Error("request sent")
		}
	})
	t.Run("unsafe service.json sends nothing", func(t *testing.T) {
		f := newFake(t, jsonOK(`{}`))
		c := NewClient(writeService(t, t.TempDir(), f.srv.URL, testPassword, 0o644))
		_, err := c.Sessions(context.Background())
		wantKind(t, err, KindUnreachable)
		if len(f.requests()) != 0 {
			t.Error("request sent")
		}
	})
}

func TestClientNotFound(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	_, err := clientFor(t, f).Session(context.Background(), sid)
	wantKind(t, err, KindNotFound)
}

func TestClientUnknownSendsOnce(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"500": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"503": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) },
		"non-json 200": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html>not json</html>"))
		},
		"empty 200": func(w http.ResponseWriter, r *http.Request) {},
		"400":       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) },
		"dropped": func(w http.ResponseWriter, r *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		},
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, h)
			c := clientFor(t, f)
			c.Timeout = 300 * time.Millisecond
			_, err := c.Prompt(context.Background(), sid, []byte(`{"text":"x"}`))
			wantKind(t, err, KindUnknown)
			if n := len(f.requests()); n != 1 {
				t.Errorf("%d requests, want exactly 1", n)
			}
		})
	}
}

func TestClientDoesNotFollowRedirect(t *testing.T) {
	second := newFake(t, jsonOK(`{}`))
	first := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	c := clientFor(t, first)
	_, err := c.Prompt(context.Background(), sid, []byte(`{"text":"secret prompt"}`))
	wantKind(t, err, KindUnknown)
	if n := len(second.requests()); n != 0 {
		t.Fatalf("second listener got %d requests: %+v", n, second.requests())
	}
	if n := len(first.requests()); n != 1 {
		t.Errorf("first listener got %d requests", n)
	}
	_, err = c.Session(context.Background(), sid)
	wantKind(t, err, KindUnknown)
	if n := len(second.requests()); n != 0 {
		t.Errorf("second listener got %d requests on GET", n)
	}
}

func TestClientRereadsPassword(t *testing.T) {
	f := newFake(t, jsonOK(`{}`))
	dir := t.TempDir()
	path := writeService(t, dir, f.srv.URL, "first-pw", 0o600)
	c := NewClient(path)
	if _, err := c.Sessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeService(t, dir, f.srv.URL, "second-pw", 0o600)
	if _, err := c.Sessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].Auth != "opencode:first-pw" || reqs[1].Auth != "opencode:second-pw" {
		t.Fatal("credential not re-read per request")
	}
}

func TestClientNeverPrintsPassword(t *testing.T) {
	f := newFake(t, jsonOK(`{}`))
	c := clientFor(t, f)
	for _, v := range []any{c, *c} {
		for _, fm := range []string{"%v", "%+v", "%#v"} {
			noPassword(t, fm, sprintf(fm, v))
		}
	}
}

func TestClientErrorsCarryNoResponseData(t *testing.T) {
	const marker = "LEAK-MARKER-9f3"
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://[bad"+marker)
		w.WriteHeader(307)
	})
	_, err := clientFor(t, f).Session(context.Background(), sid)
	e := wantKind(t, err, KindUnknown)
	for _, s := range []string{e.Error(), fmt.Sprintf("%#v", e), fmt.Sprintf("%+v", e)} {
		if strings.Contains(s, marker) {
			t.Errorf("response data in error: %q", s)
		}
	}
}

func TestClient401WithBrokenBodyIsUnreachable(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(401)
		_, _ = w.Write([]byte("x"))
	})
	_, err := clientFor(t, f).Session(context.Background(), sid)
	wantKind(t, err, KindUnreachable)
}

func TestClientOversizedBodyIsUnknown(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"` + strings.Repeat("a", maxBody) + `"`))
	})
	_, err := clientFor(t, f).Session(context.Background(), sid)
	wantKind(t, err, KindUnknown)
}

func TestClientPinOrigin(t *testing.T) {
	f := newFake(t, jsonOK(`{}`))
	c := clientFor(t, f)
	c.PinOrigin = "http://127.0.0.1:1"
	_, err := c.Sessions(context.Background())
	wantKind(t, err, KindUnreachable)
	if len(f.requests()) != 0 {
		t.Error("request sent to an unpinned origin")
	}
	c.PinOrigin = f.srv.URL
	if _, err := c.Sessions(context.Background()); err != nil {
		t.Fatal(err)
	}
}
