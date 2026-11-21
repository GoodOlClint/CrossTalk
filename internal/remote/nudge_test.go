package remote

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func TestAddressChangedPredicate(t *testing.T) {
	base := Config{Alias: "a", Bind: "10.0.0.1", Port: 7745}
	cases := []struct {
		name       string
		old, next  Config
		configured bool
		want       bool
	}{
		{"advertise changed", base, Config{Alias: "a", Bind: "10.0.0.1", Advertise: "h.example", Port: 7745}, true, true},
		{"bind changed with no advertise", base, Config{Alias: "a", Bind: "10.0.0.2", Port: 7745}, true, true},
		{"port changed", base, Config{Alias: "a", Bind: "10.0.0.1", Port: 7746}, true, true},
		{"alias only", base, Config{Alias: "b", Bind: "10.0.0.1", Port: 7745}, true, false},
		{"identical rewrite", base, base, true, false},
		{"bind moved but advertise holds", Config{Alias: "a", Bind: "10.0.0.1", Advertise: "h.example", Port: 7745}, Config{Alias: "a", Bind: "10.0.0.2", Advertise: "h.example", Port: 7745}, true, false},
		{"first configure", Config{}, base, false, false},
	}
	for _, c := range cases {
		if got := AddressChanged(c.old, c.next, c.configured); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

type nudgeRec struct {
	mu     sync.Mutex
	local  map[string]string
	remote map[string][]msg.Message
}

func (r *nudgeRec) deps(peers []Peer, rows map[string][]AgentRow) NudgeDeps {
	r.local, r.remote = map[string]string{}, map[string][]msg.Message{}
	return NudgeDeps{
		SendLocal: func(d registry.Descriptor, text string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.local[d.AgentID] = text
			return nil
		},
		Host:  Host{Fingerprint: strings.Repeat("ab", 32)},
		Peers: peers,
		Rows: func(ctx context.Context, h Host, p Peer) ([]AgentRow, error) {
			if rs, ok := rows[p.Alias]; ok {
				return rs, nil
			}
			return nil, errors.New("unreachable")
		},
		SendRemote: func(ctx context.Context, from registry.Descriptor, h Host, p Peer, id string, m msg.Message) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.remote[p.Alias+"/"+id] = append(r.remote[p.Alias+"/"+id], m)
			return nil
		},
		Timeout: 2 * time.Second,
	}
}

var (
	nudgeOld  = Config{Alias: "lab", Bind: "10.0.0.1", Port: 7745}
	nudgeNext = Config{Alias: "lab", Bind: "10.0.0.2", Port: 7745}
)

func TestNudgeLocalSessionsGetIt(t *testing.T) {
	var r nudgeRec
	d := r.deps(nil, nil)
	d.Locals = []registry.Descriptor{
		{AgentID: "1111111111111111", Name: "one", UDSPath: "/s/1"},
		{AgentID: "2222222222222222", Name: "two", UDSPath: ""},
		{AgentID: "3333333333333333", Name: "three", UDSPath: "/s/3"},
	}
	base := d.SendLocal
	d.SendLocal = func(x registry.Descriptor, text string) error {
		base(x, text)
		if x.Name == "one" {
			return errors.New("socket gone")
		}
		return nil
	}
	SendAddressNudge(context.Background(), d, "lab", nudgeOld, nudgeNext)
	if len(r.local) != 2 || r.local["1111111111111111"] == "" || r.local["3333333333333333"] == "" {
		t.Fatalf("local deliveries %v, want ids 1 and 3 only", r.local)
	}
	if _, ok := r.local["2222222222222222"]; ok {
		t.Error("a session with no socket was sent to")
	}
}

func TestNudgePairedHostSessionsGetIt(t *testing.T) {
	var r nudgeRec
	d := r.deps([]Peer{{Alias: "desk"}, {Alias: "den"}}, map[string][]AgentRow{
		"desk": {{AgentID: "aaaaaaaaaaaaaaaa", CrossTalk: true, Messageable: true}, {AgentID: "bbbbbbbbbbbbbbbb", CrossTalk: false}},
		"den":  {{AgentID: "cccccccccccccccc", CrossTalk: true, Messageable: true}},
	})
	SendAddressNudge(context.Background(), d, "lab", nudgeOld, nudgeNext)
	if len(r.remote) != 2 || len(r.remote["desk/aaaaaaaaaaaaaaaa"]) != 1 || len(r.remote["den/cccccccccccccccc"]) != 1 {
		t.Fatalf("remote deliveries %v, want one each for desk/aaaa and den/cccc", r.remote)
	}
	if _, ok := r.remote["desk/bbbbbbbbbbbbbbbb"]; ok {
		t.Error("a native row was sent to")
	}
}

func TestNudgeUnreachablePeerGetsNothingAndDoesNotDelayLocal(t *testing.T) {
	var r nudgeRec
	d := r.deps([]Peer{{Alias: "down"}, {Alias: "hung"}, {Alias: "ok"}}, nil)
	d.Locals = []registry.Descriptor{{AgentID: "1111111111111111", Name: "one", UDSPath: "/s/1"}}
	d.Timeout = 300 * time.Millisecond
	release := make(chan struct{})
	d.Rows = func(ctx context.Context, h Host, p Peer) ([]AgentRow, error) {
		switch p.Alias {
		case "down":
			return nil, errors.New("unreachable")
		case "ok":
			return []AgentRow{{AgentID: "aaaaaaaaaaaaaaaa", CrossTalk: true, Messageable: true}}, nil
		}
		select {
		case <-ctx.Done():
		case <-release:
		}
		return nil, ctx.Err()
	}
	localAt := make(chan time.Time, 1)
	base := d.SendLocal
	d.SendLocal = func(x registry.Descriptor, text string) error {
		localAt <- time.Now()
		return base(x, text)
	}
	start := time.Now()
	SendAddressNudge(context.Background(), d, "lab", nudgeOld, nudgeNext)
	took := time.Since(start)
	close(release)
	if got := <-localAt; got.Sub(start) > 100*time.Millisecond {
		t.Errorf("local nudge started %v after the call, behind a hung peer", got.Sub(start))
	}
	if took > 2*time.Second {
		t.Errorf("call took %v with a 300ms budget", took)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.local) != 1 || len(r.remote) != 1 || len(r.remote["ok/aaaaaaaaaaaaaaaa"]) != 1 {
		t.Errorf("local %v remote %v, want one local and only the reachable peer's session", r.local, r.remote)
	}
}

func TestNudgeSenderIsInformAndNeverCommand(t *testing.T) {
	var r nudgeRec
	d := r.deps([]Peer{{Alias: "desk"}}, map[string][]AgentRow{"desk": {{AgentID: "aaaaaaaaaaaaaaaa", CrossTalk: true, Messageable: true}}})
	SendAddressNudge(context.Background(), d, "lab", nudgeOld, nudgeNext)
	ms := r.remote["desk/aaaaaaaaaaaaaaaa"]
	if len(ms) != 1 {
		t.Fatalf("got %d messages", len(ms))
	}
	m := ms[0]
	if m.Kind != msg.Inform {
		t.Errorf("kind %q, want inform", m.Kind)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("message does not validate: %v", err)
	}
}

func TestNudgeSenderNameAvoidsLocalNames(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	taken := map[string]bool{nudgeSenderName: true, nudgeSenderName + "-2": true}
	s := NudgeSender(fp, func(n string) bool { return taken[n] })
	if taken[s.Name] {
		t.Errorf("name %q collides with a local name", s.Name)
	}
	enc := func(n string) string { return base64URL(n) }
	if _, ok := a2a.ValidateAttested(s.AgentID, enc(s.Name), s.Harness, s.Role); !ok {
		t.Errorf("sender %+v does not pass attestation", s)
	}
	if s2 := NudgeSender(fp, nil); s2.AgentID != s.AgentID {
		t.Errorf("agent_id changed between calls: %s vs %s", s2.AgentID, s.AgentID)
	}
	if other := NudgeSender(strings.Repeat("cd", 32), nil); other.AgentID == s.AgentID {
		t.Error("two hosts share a sender agent_id")
	}

	var r nudgeRec
	d := r.deps([]Peer{{Alias: "desk"}}, map[string][]AgentRow{"desk": {{AgentID: "aaaaaaaaaaaaaaaa", CrossTalk: true, Messageable: true}}})
	d.Locals = []registry.Descriptor{{AgentID: "1111111111111111", Name: nudgeSenderName}}
	SendAddressNudge(context.Background(), d, "lab", nudgeOld, nudgeNext)
	if got := r.remote["desk/aaaaaaaaaaaaaaaa"][0].From; got == nudgeSenderName {
		t.Errorf("From %q equals a local session name", got)
	}
}

func TestNudgeTextSaysItIsNotASession(t *testing.T) {
	for _, txt := range []string{LocalNudgeText(nudgeOld, nudgeNext), PeerNudgeText("lab", nudgeOld, nudgeNext)} {
		for _, want := range []string{"`crosstalk remote config`", "not from a session", "10.0.0.1:7745", "10.0.0.2:7745"} {
			if !strings.Contains(txt, want) {
				t.Errorf("text %q lacks %q", txt, want)
			}
		}
	}
}

func TestNudgeBoundsRowsPerPeer(t *testing.T) {
	var rows []AgentRow
	for i := 0; i < nudgeMaxRows+30; i++ {
		rows = append(rows, AgentRow{AgentID: strings.Repeat("a", 12) + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "aa", CrossTalk: true, Messageable: true})
	}
	var r nudgeRec
	d := r.deps([]Peer{{Alias: "desk"}}, map[string][]AgentRow{"desk": rows})
	SendAddressNudge(context.Background(), d, "lab", nudgeOld, nudgeNext)
	if len(r.remote) != nudgeMaxRows {
		t.Errorf("%d rows messaged, want %d", len(r.remote), nudgeMaxRows)
	}
}

func base64URL(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
