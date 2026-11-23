package opencodetest

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const Password = "PW-s3cr3t-Zk9Qx"

type Request struct{ Method, Path, Body string }

// Server serves a Service over HTTP and records every request it receives.
type Server struct {
	*Service
	URL string
	// Close stops the HTTP listener.
	Close func()

	mu   sync.Mutex
	reqs []Request
}

func Start(t testing.TB, sessions ...Session) *Server {
	t.Helper()
	s := &Server{Service: NewService(sessions...)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		s.mu.Lock()
		s.reqs = append(s.reqs, Request{r.Method, r.URL.Path, string(b)})
		s.mu.Unlock()
		s.Handle(w, r)
	}))
	t.Cleanup(srv.Close)
	s.URL, s.Close = srv.URL, srv.Close
	return s
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Prompts returns the bodies of the POST .../prompt requests received.
func (s *Server) Prompts() []string {
	var out []string
	for _, q := range s.Requests() {
		if q.Method == http.MethodPost && strings.HasSuffix(q.Path, "/prompt") {
			out = append(out, q.Body)
		}
	}
	return out
}

// WriteService writes a service.json for url into dir and returns its path.
func WriteService(t testing.TB, dir, url, version string, pid int) string {
	t.Helper()
	path := filepath.Join(dir, "service.json")
	body := fmt.Sprintf(`{"id":"svc_1","version":%q,"url":%q,"pid":%d,"password":%q}`, version, url, pid, Password)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
