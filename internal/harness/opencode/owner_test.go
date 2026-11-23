package opencode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func TestMain(m *testing.M) {
	if os.Getenv("CROSSTALK_TEST_SQUATTER") == "1" {
		squatter()
		return
	}
	listenerPIDs = func(context.Context, int) ([]int, error) { return []int{4242}, nil }
	os.Exit(m.Run())
}

type ownerSeam struct {
	mu    sync.Mutex
	ports []int
	next  func(call int) ([]int, error)
}

func setOwner(t *testing.T, next func(call int) ([]int, error)) *ownerSeam {
	t.Helper()
	s := &ownerSeam{next: next}
	old := listenerPIDs
	listenerPIDs = func(_ context.Context, port int) ([]int, error) {
		s.mu.Lock()
		s.ports = append(s.ports, port)
		n := len(s.ports)
		s.mu.Unlock()
		return s.next(n)
	}
	t.Cleanup(func() { listenerPIDs = old })
	return s
}

func owners(pids ...int) func(int) ([]int, error) {
	return func(int) ([]int, error) { return pids, nil }
}

func (s *ownerSeam) calls() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.ports...)
}

func portOf(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func refusedOwner(t *testing.T, r *rig, seam *ownerSeam) {
	t.Helper()
	ctx := context.Background()
	st, err := r.h.Inject(ctx, peerMsg(msg.Immediate), teammate(r))
	if st != adapter.Unreachable || err == nil {
		t.Fatalf("Inject = %v, %v; want unreachable", st, err)
	}
	clean(t, "inject error", err)
	if err := r.h.MailSignal(ctx, peerMsg(msg.Immediate), scriptSender(r)); err == nil {
		t.Fatal("MailSignal sent through a listener the service does not own")
	} else {
		clean(t, "signal error", err)
	}
	c := NewClient(r.path)
	for name, do := range map[string]func() (*Response, error){
		"sessions": func() (*Response, error) { return c.Sessions(ctx) },
		"active":   func() (*Response, error) { return c.ActiveSessions(ctx) },
		"session":  func() (*Response, error) { return c.Session(ctx, sid) },
		"prompt":   func() (*Response, error) { return c.Prompt(ctx, sid, []byte(`{}`)) },
	} {
		_, err := do()
		e := wantKind(t, err, KindUnreachable)
		if !strings.Contains(e.Msg, "nothing sent") {
			t.Errorf("%s: %q does not say nothing was sent", name, e.Msg)
		}
	}
	if reqs := r.f.requests(); len(reqs) != 0 {
		t.Fatalf("%d requests reached the listener: %+v", len(reqs), reqs)
	}
	want := portOf(t, r.f.srv.URL)
	got := seam.calls()
	if len(got) == 0 {
		t.Fatal("owner lookup never ran")
	}
	for _, p := range got {
		if p != want {
			t.Fatalf("owner looked up on port %d, request goes to %d", p, want)
		}
	}
}

func TestOwnerRefusesForeignListener(t *testing.T) {
	cases := map[string][]int{
		"squatter":                {9999},
		"squatter beside service": {4242, 9999},
	}
	for name, pids := range cases {
		t.Run(name, func(t *testing.T) {
			seam := setOwner(t, owners(pids...))
			refusedOwner(t, newRig(t, "2.4.0", nil), seam)
		})
	}
}

func TestOwnerRefusesUndeterminable(t *testing.T) {
	cases := map[string]func(int) ([]int, error){
		"lookup error":      func(int) ([]int, error) { return nil, errors.New("lsof: exec failed") },
		"no listener":       owners(),
		"permission denied": func(int) ([]int, error) { return nil, os.ErrPermission },
		"error with pids":   func(int) ([]int, error) { return []int{4242}, errors.New("partial") },
	}
	for name, next := range cases {
		t.Run(name, func(t *testing.T) {
			seam := setOwner(t, next)
			refusedOwner(t, newRig(t, "2.4.0", nil), seam)
		})
	}
}

func TestOwnerMatchSends(t *testing.T) {
	seam := setOwner(t, owners(4242))
	r := newRig(t, "2.4.0", nil)
	st, err := r.h.Inject(context.Background(), peerMsg(msg.Immediate), teammate(r))
	if st != adapter.Accepted || err != nil {
		t.Fatalf("Inject = %v, %v", st, err)
	}
	if err := r.h.MailSignal(context.Background(), peerMsg(msg.Immediate), scriptSender(r)); err != nil {
		t.Fatalf("MailSignal: %v", err)
	}
	if n := len(r.prompts()); n != 2 {
		t.Fatalf("%d prompts, want 2", n)
	}
	if n := len(seam.calls()); n != 2 {
		t.Fatalf("%d owner lookups, want one per request", n)
	}
}

func TestOwnerFollowsRestartedService(t *testing.T) {
	pid := 4242
	setOwner(t, func(int) ([]int, error) { return []int{pid}, nil })
	r := newRig(t, "2.4.0", nil)
	d := teammate(r)
	if st, err := r.h.Inject(context.Background(), peerMsg(msg.Immediate), d); st != adapter.Accepted {
		t.Fatalf("before restart: %v %v", st, err)
	}
	pid = 5151
	writeSvc(t, strings.TrimSuffix(r.path, "/service.json"), r.f.srv.URL, "2.4.0", 5151)
	if st, err := r.h.Inject(context.Background(), peerMsg(msg.Immediate), d); st != adapter.Accepted {
		t.Fatalf("after restart with a rewritten service.json: %v %v", st, err)
	}
	if n := len(r.prompts()); n != 2 {
		t.Fatalf("%d prompts, want 2", n)
	}
}

func TestOwnerCheckedEveryRequest(t *testing.T) {
	seam := setOwner(t, func(call int) ([]int, error) {
		if call == 1 {
			return []int{4242}, nil
		}
		return []int{9999}, nil
	})
	r := newRig(t, "2.4.0", nil)
	d := teammate(r)
	if st, err := r.h.Inject(context.Background(), peerMsg(msg.Immediate), d); st != adapter.Accepted {
		t.Fatalf("first send: %v %v", st, err)
	}
	if st, _ := r.h.Inject(context.Background(), peerMsg(msg.Immediate), d); st != adapter.Unreachable {
		t.Fatalf("second send after the owner changed: %v, want unreachable", st)
	}
	if n := len(r.prompts()); n != 1 {
		t.Fatalf("%d prompts, want 1", n)
	}
	if n := len(seam.calls()); n != 2 {
		t.Fatalf("%d owner lookups, want 2", n)
	}
}

func TestOSListenerPIDs(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	pids, err := osListenerPIDs(context.Background(), port)
	if err != nil || len(pids) != 1 || pids[0] != os.Getpid() {
		t.Fatalf("own listener: %v %v, want [%d]", pids, err, os.Getpid())
	}
	ln.Close()
	if pids, _ := osListenerPIDs(context.Background(), port); len(pids) != 0 {
		t.Fatalf("closed port: %v, want none", pids)
	}
}

func TestOwnerRealSquatter(t *testing.T) {
	old := listenerPIDs
	listenerPIDs = osListenerPIDs
	t.Cleanup(func() { listenerPIDs = old })

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "CROSSTALK_TEST_SQUATTER=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(out)
	if !lines.Scan() {
		t.Fatal("squatter printed no address")
	}
	addr := lines.Text()
	got := make(chan []string, 1)
	go func() {
		var seen []string
		for lines.Scan() {
			seen = append(seen, lines.Text())
		}
		got <- seen
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	origin := "http://" + addr
	path := writeSvc(t, t.TempDir(), origin, "2.4.0", os.Getpid())
	h := NewHarness(Options{ServicePath: path})
	d := adapter.Delivery{Target: adapter.Binding{Harness: adapter.OpenCode, SessionRef: sid, HarnessEndpoint: origin},
		VerifiedSender: true, SenderHarness: "claude", Notice: msg.NoticeTeammate}
	st, err := h.Inject(context.Background(), peerMsg(msg.Immediate), d)
	if st != adapter.Unreachable || err == nil {
		t.Errorf("Inject to a squatter = %v, %v; want unreachable", st, err)
	}
	if err := h.MailSignal(context.Background(), peerMsg(msg.Immediate), d); err == nil {
		t.Error("MailSignal to a squatter succeeded")
	}
	in.Close()
	select {
	case seen := <-got:
		if len(seen) != 0 {
			t.Fatalf("squatter received %d requests: %q", len(seen), seen)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("squatter did not exit")
	}
}

func squatter() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	var mu sync.Mutex
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fmt.Printf("%s %s auth=%t\n", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"m1","sessionID":"` + sid + `","delivery":"steer"}}`))
	})}
	fmt.Println(ln.Addr().String())
	go func() { _ = srv.Serve(ln) }()
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	_ = srv.Close()
	mu.Lock()
	os.Exit(0)
}
