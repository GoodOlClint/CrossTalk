package opencode

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

const peerWords = "PEER-WORDS-7f3a"

func peerMsg(d msg.Delivery) msg.Message {
	return msg.Message{MessageID: "msg_in_1", From: "peer-a", Kind: msg.Inform, Delivery: d,
		Parts: []msg.Part{{Type: "text", Text: peerWords}}}
}

func teammate(r *rig) adapter.Delivery {
	return adapter.Delivery{Target: r.binding(), VerifiedSender: true, SenderHarness: "claude", Notice: msg.NoticeTeammate}
}

func (r *rig) binding() adapter.Binding {
	return adapter.Binding{Harness: adapter.OpenCode, SessionRef: sid, HarnessEndpoint: r.f.srv.URL, HarnessVersion: "2.4.0", HarnessPID: 4242}
}

func (r *rig) prompts() []recorded {
	var out []recorded
	for _, q := range r.f.requests() {
		if q.Method == http.MethodPost {
			out = append(out, q)
		}
	}
	return out
}

func promptBody(t *testing.T, q recorded) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal([]byte(q.Body), &b); err != nil {
		t.Fatalf("prompt body %q: %v", q.Body, err)
	}
	return b
}

// hygiene pins ADR 0033: a plain prompt only, on the prompt path only.
func hygiene(t *testing.T, r *rig) {
	t.Helper()
	noTUI(t, r.f)
	for _, q := range r.f.requests() {
		if strings.Contains(strings.ToLower(q.Path), "instructions") {
			t.Errorf("instructions path %s", q.Path)
		}
		if q.Method != http.MethodPost {
			continue
		}
		if q.Path != "/api/session/"+sid+"/prompt" {
			t.Errorf("POST to %s", q.Path)
		}
		b := promptBody(t, q)
		for _, k := range []string{"system", "synthetic", "resume", "parts", "noReply"} {
			if _, ok := b[k]; ok {
				t.Errorf("prompt body has %q: %s", k, q.Body)
			}
		}
		if len(b) != 2 {
			t.Errorf("prompt body has fields beyond text and delivery: %s", q.Body)
		}
	}
}

func TestInjectSteerAndQueue(t *testing.T) {
	cases := []struct {
		name     string
		mode     msg.Delivery
		msgMode  msg.Delivery
		delivery string
		want     adapter.State
	}{
		{"immediate", msg.Immediate, "", "steer", adapter.Accepted},
		{"next_turn", msg.NextTurn, "", "queue", adapter.Queued},
		{"mode from message", "", msg.NextTurn, "queue", adapter.Queued},
		{"default", "", "", "steer", adapter.Accepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "2.4.0", nil)
			d := teammate(r)
			d.Mode = tc.mode
			m := peerMsg(tc.msgMode)
			for _, kind := range []msg.Kind{msg.Inform, msg.Ask, msg.Command} {
				m.Kind = kind
				st, err := r.h.Inject(context.Background(), m, d)
				if err != nil || st != tc.want {
					t.Fatalf("%s: %v %v, want %v", kind, st, err, tc.want)
				}
			}
			ps := r.prompts()
			if len(ps) != 3 {
				t.Fatalf("%d POSTs, want 3", len(ps))
			}
			for _, q := range ps {
				b := promptBody(t, q)
				if b["delivery"] != tc.delivery {
					t.Errorf("delivery %v, want %s", b["delivery"], tc.delivery)
				}
				text, _ := b["text"].(string)
				if !strings.HasPrefix(text, Preamble+"\n") || !strings.Contains(text, peerWords) {
					t.Errorf("text %q is not the rendered message", text)
				}
				if q.Auth != "opencode:"+testPassword {
					t.Error("wrong credential")
				}
			}
			want := RenderIn(m, true, "claude", "", "", false, msg.NoticeTeammate)
			if got := promptBody(t, ps[2])["text"]; got != want {
				t.Errorf("text %q, want %q", got, want)
			}
			hygiene(t, r)
		})
	}
}

func refusedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	_ = l.Close()
	return u
}

func TestInjectFailureStates(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *rig, d *adapter.Delivery)
		want  adapter.State
		posts int
	}{
		{"401", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.set(func(s *service) { s.prompt.Status = 401 }) }, adapter.Unreachable, 1},
		{"404", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.set(func(s *service) { s.prompt.Status = 404 }) }, adapter.Unreachable, 1},
		{"refused connection", func(t *testing.T, r *rig, d *adapter.Delivery) {
			u := refusedURL(t)
			r.h = NewHarness(Options{ServicePath: writeSvc(t, t.TempDir(), u, "2.4.0", 4242)})
			d.Target.HarnessEndpoint = u
		}, adapter.Unreachable, 0},
		{"missing service.json", func(t *testing.T, r *rig, _ *adapter.Delivery) {
			r.h = NewHarness(Options{ServicePath: filepath.Join(t.TempDir(), "service.json")})
		}, adapter.Unreachable, 0},
		{"refused major", func(t *testing.T, r *rig, _ *adapter.Delivery) {
			r.h = NewHarness(Options{ServicePath: writeSvc(t, t.TempDir(), r.f.srv.URL, "3.0.0", 4242)})
		}, adapter.Unreachable, 0},
		{"origin differs from binding", func(_ *testing.T, _ *rig, d *adapter.Delivery) { d.Target.HarnessEndpoint = "http://127.0.0.1:1" }, adapter.Unreachable, 0},
		{"bad session id", func(_ *testing.T, _ *rig, d *adapter.Delivery) { d.Target.SessionRef = "ses_../x" }, adapter.Unreachable, 0},
		{"500 after send", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.set(func(s *service) { s.prompt.Status = 500 }) }, adapter.Unknown, 1},
		{"503 after send", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.set(func(s *service) { s.prompt.Status = 503 }) }, adapter.Unknown, 1},
		{"dropped response", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.set(func(s *service) { s.prompt.Drop = true }) }, adapter.Unknown, 1},
		{"success names another session", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.set(func(s *service) { s.prompt.Session = "ses_other9" })
		}, adapter.Unknown, 1},
		{"non-JSON 200", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.set(func(s *service) { s.prompt.Raw = "ok, admitted" })
		}, adapter.Unknown, 1},
		{"200 without data", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.set(func(s *service) { s.prompt.Raw = `{"ok":true}` })
		}, adapter.Unknown, 1},
		{"200 without message id", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.set(func(s *service) { s.prompt.Raw = `{"data":{"sessionID":"` + sid + `","delivery":"steer"}}` })
		}, adapter.Unknown, 1},
		{"200 with other delivery", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.set(func(s *service) {
				s.prompt.Raw = `{"data":{"id":"msg_1","sessionID":"` + sid + `","delivery":"later"}}`
			})
		}, adapter.Unknown, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "2.4.0", nil)
			d := teammate(r)
			tc.setup(t, r, &d)
			st, err := r.h.Inject(context.Background(), peerMsg(msg.Immediate), d)
			if st != tc.want {
				t.Fatalf("state %v (%v), want %v", st, err, tc.want)
			}
			if err == nil {
				t.Fatal("no error for a failed delivery")
			}
			if n := len(r.prompts()); n != tc.posts {
				t.Fatalf("%d POSTs, want %d", n, tc.posts)
			}
			clean(t, "state", st)
			clean(t, "error", err)
			hygiene(t, r)
		})
	}
}

func TestInjectD15Matrix(t *testing.T) {
	cases := []struct {
		name string
		d    func(adapter.Delivery) adapter.Delivery
		want adapter.State
	}{
		{"verified local teammate", func(d adapter.Delivery) adapter.Delivery { return d }, adapter.Accepted},
		{"verified local teammate again", func(d adapter.Delivery) adapter.Delivery { d.Notice = msg.NoticeTeammateAgain; return d }, adapter.Accepted},
		{"remote sender", func(d adapter.Delivery) adapter.Delivery { d.SenderRemote, d.Notice = true, msg.NoticeInfo; return d }, adapter.Mailboxed},
		{"remote sender with teammate notice", func(d adapter.Delivery) adapter.Delivery { d.SenderRemote = true; return d }, adapter.Mailboxed},
		{"local-user script sender", func(d adapter.Delivery) adapter.Delivery { d.SenderHarness, d.Notice = "", msg.NoticeInfo; return d }, adapter.Mailboxed},
		{"unverified sender", func(d adapter.Delivery) adapter.Delivery {
			d.VerifiedSender, d.Notice = false, msg.NoticeInfo
			return d
		}, adapter.Mailboxed},
		{"unverified with teammate notice", func(d adapter.Delivery) adapter.Delivery { d.VerifiedSender = false; return d }, adapter.Mailboxed},
		{"inbox copy", func(d adapter.Delivery) adapter.Delivery {
			d.InboxCopy, d.Inbox, d.Notice = true, "builders", msg.NoticeCopy
			return d
		}, adapter.Mailboxed},
		{"inbox copy with teammate notice", func(d adapter.Delivery) adapter.Delivery { d.InboxCopy = true; return d }, adapter.Mailboxed},
		{"no notice class", func(d adapter.Delivery) adapter.Delivery { d.Notice = ""; return d }, adapter.Mailboxed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "2.4.0", nil)
			st, err := r.h.Inject(context.Background(), peerMsg(msg.Immediate), tc.d(teammate(r)))
			if err != nil || st != tc.want {
				t.Fatalf("%v %v, want %v", st, err, tc.want)
			}
			n := len(r.f.requests())
			if tc.want == adapter.Mailboxed && n != 0 {
				t.Fatalf("mailboxed delivery sent %d requests", n)
			}
			if tc.want != adapter.Mailboxed && len(r.prompts()) != 1 {
				t.Fatalf("raw delivery sent %d POSTs", len(r.prompts()))
			}
			hygiene(t, r)
		})
	}
}
