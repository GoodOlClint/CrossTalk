package opencode

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

const peerWords = "PEER-WORDS-7f3a"

func peerMsg(d msg.Delivery) msg.Message {
	return msg.Message{MessageID: "msg_in_1", From: "peer-a", Kind: msg.Inform, Delivery: d,
		Parts: []msg.Part{{Type: "text", Text: peerWords}}}
}

func liveSession() fakeSession { return fakeSession{ID: sid, Title: "S", Updated: t0} }

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
		for _, k := range []string{"system", "synthetic", "parts", "noReply"} {
			if _, ok := b[k]; ok {
				t.Errorf("prompt body has %q: %s", k, q.Body)
			}
		}
		want := 2
		if b["delivery"] == "queue" {
			want = 3
		}
		if len(b) != want {
			t.Errorf("prompt body has fields beyond text, delivery and resume: %s", q.Body)
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
			r := newRig(t, "2.4.0", nil, liveSession())
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
		{"401", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.Set(func(s *service) { s.Prompt.Status = 401 }) }, adapter.Unreachable, 1},
		{"404", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.Set(func(s *service) { s.Prompt.Status = 404 }) }, adapter.Unreachable, 1},
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
		{"500 after send", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.Set(func(s *service) { s.Prompt.Status = 500 }) }, adapter.Unknown, 1},
		{"503 after send", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.Set(func(s *service) { s.Prompt.Status = 503 }) }, adapter.Unknown, 1},
		{"dropped response", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.Set(func(s *service) { s.Prompt.Drop = true }) }, adapter.Unknown, 1},
		{"success names another session", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.Set(func(s *service) { s.Prompt.Session = "ses_other9" })
		}, adapter.Unknown, 1},
		{"non-JSON 200", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.Set(func(s *service) { s.Prompt.Raw = "ok, admitted" })
		}, adapter.Unknown, 1},
		{"200 without data", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.Set(func(s *service) { s.Prompt.Raw = `{"ok":true}` })
		}, adapter.Unknown, 1},
		{"200 without message id", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.Set(func(s *service) { s.Prompt.Raw = `{"data":{"sessionID":"` + sid + `","delivery":"steer"}}` })
		}, adapter.Unknown, 1},
		{"200 with other delivery", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.Set(func(s *service) {
				s.Prompt.Raw = `{"data":{"id":"msg_1","sessionID":"` + sid + `","delivery":"later"}}`
			})
		}, adapter.Unknown, 1},
		{"202 with admitted fields", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.Set(func(s *service) {
				s.Prompt.Raw, s.Prompt.RawStatus = `{"data":{"id":"msg_1","sessionID":"`+sid+`","delivery":"steer"}}`, 202
			})
		}, adapter.Unknown, 1},
		{"200 without delivery", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.Set(func(s *service) { s.Prompt.Raw = `{"data":{"id":"msg_1","sessionID":"` + sid + `"}}` })
		}, adapter.Unknown, 1},
		{"200 with null delivery", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.Set(func(s *service) { s.Prompt.Raw = `{"data":{"id":"msg_1","sessionID":"` + sid + `","delivery":null}}` })
		}, adapter.Unknown, 1},
		{"binding has no endpoint", func(_ *testing.T, _ *rig, d *adapter.Delivery) { d.Target.HarnessEndpoint = "" }, adapter.Unreachable, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "2.4.0", nil, liveSession())
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
			r := newRig(t, "2.4.0", nil, liveSession())
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

func TestInjectCanceledBeforeSend(t *testing.T) {
	r := newRig(t, "2.4.0", nil, liveSession())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st, err := r.h.Inject(ctx, peerMsg(msg.Immediate), teammate(r))
	if st != adapter.Unreachable || err == nil {
		t.Fatalf("%v %v, want unreachable", st, err)
	}
	if n := len(r.f.requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestInjectNextTurnStoresWithoutATurn(t *testing.T) {
	r := newRig(t, "2.4.0", nil, liveSession())
	d := teammate(r)
	d.Mode = msg.NextTurn
	st, err := r.h.Inject(context.Background(), peerMsg(""), d)
	if err != nil || st != adapter.Queued {
		t.Fatalf("%v %v, want queued", st, err)
	}
	ps := r.prompts()
	if len(ps) != 1 {
		t.Fatalf("%d POSTs, want 1", len(ps))
	}
	b := promptBody(t, ps[0])
	if b["delivery"] != "queue" || b["resume"] != false {
		t.Errorf("body %s, want delivery queue and resume false", ps[0].Body)
	}
	hygiene(t, r)
}

func TestInjectImmediateHasNoResume(t *testing.T) {
	r := newRig(t, "2.4.0", nil, liveSession())
	d := teammate(r)
	d.Mode = msg.Immediate
	st, err := r.h.Inject(context.Background(), peerMsg(""), d)
	if err != nil || st != adapter.Accepted {
		t.Fatalf("%v %v, want accepted", st, err)
	}
	b := promptBody(t, r.prompts()[0])
	if _, ok := b["resume"]; ok || b["delivery"] != "steer" {
		t.Errorf("body %v, want steer with no resume field", b)
	}
}

func TestInjectArchivedSession(t *testing.T) {
	cases := []struct {
		name     string
		sessions []fakeSession
		status   int
		want     adapter.State
		posts    int
	}{
		{"archived", []fakeSession{{ID: sid, Title: "S", Updated: t0, Archived: true}}, 0, adapter.Unreachable, 0},
		{"not archived", []fakeSession{liveSession()}, 0, adapter.Accepted, 1},
		{"state read fails", []fakeSession{liveSession()}, 500, adapter.Unreachable, 0},
		{"session gone", nil, 0, adapter.Unreachable, 0},
	}
	for _, tc := range cases {
		for _, mode := range []msg.Delivery{msg.Immediate, msg.NextTurn} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				r := newRig(t, "2.4.0", nil, tc.sessions...)
				r.svc.Set(func(s *service) { s.Status = tc.status })
				d := teammate(r)
				d.Mode = mode
				st, err := r.h.Inject(context.Background(), peerMsg(""), d)
				want := tc.want
				if want == adapter.Accepted && mode == msg.NextTurn {
					want = adapter.Queued
				}
				if st != want {
					t.Fatalf("state %v (%v), want %v", st, err, want)
				}
				if (err == nil) != (want == adapter.Accepted || want == adapter.Queued) {
					t.Fatalf("err %v for state %v", err, st)
				}
				if n := len(r.prompts()); n != tc.posts {
					t.Fatalf("%d POSTs, want %d", n, tc.posts)
				}
			})
		}
	}
}

func TestInjectArchiveStateUnreadable(t *testing.T) {
	for _, raw := range []string{`"yes"`, `-5`, `0`, `{}`, `true`} {
		for _, mode := range []msg.Delivery{msg.Immediate, msg.NextTurn} {
			t.Run(raw+"/"+string(mode), func(t *testing.T) {
				r := newRig(t, "2.4.0", nil, fakeSession{ID: sid, Title: "S", Updated: t0, RawArchived: raw})
				d := teammate(r)
				d.Mode = mode
				if st, err := r.h.Inject(context.Background(), peerMsg(""), d); st != adapter.Unreachable || err == nil {
					t.Fatalf("Inject %v %v, want unreachable", st, err)
				}
				if st, _ := r.h.Inject(context.Background(), peerMsg(""), d); st != adapter.Unreachable {
					t.Fatalf("second Inject %v", st)
				}
				if err := r.h.MailSignal(context.Background(), peerMsg(""), scriptSender(r)); err == nil {
					t.Fatal("MailSignal sent to a session with unreadable archive state")
				}
				if n := len(r.prompts()); n != 0 {
					t.Fatalf("%d POSTs, want 0", n)
				}
			})
		}
	}
}

func TestMailSignalArchivedSendsNothing(t *testing.T) {
	r := newRig(t, "2.4.0", nil, fakeSession{ID: sid, Title: "S", Updated: t0, Archived: true})
	if err := r.h.MailSignal(context.Background(), peerMsg(""), scriptSender(r)); err == nil {
		t.Fatal("no error for an archived session")
	}
	if n := len(r.prompts()); n != 0 {
		t.Fatalf("%d POSTs, want 0", n)
	}
}

func TestInjectOneDeadlineForBothRequests(t *testing.T) {
	r := newRig(t, "2.4.0", nil, liveSession())
	r.h.client.Timeout = 400 * time.Millisecond
	r.svc.Set(func(s *service) { s.GetDelay = 300 * time.Millisecond; s.Prompt.Delay = 10 * time.Second })
	start := time.Now()
	st, _ := r.h.Inject(context.Background(), peerMsg(""), teammate(r))
	if st != adapter.Unknown {
		t.Fatalf("state %v, want unknown (request left, no admission)", st)
	}
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Fatalf("Inject took %v, want one 400ms deadline over both requests", el)
	}
}
