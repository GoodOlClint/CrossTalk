package opencode

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func scriptSender(r *rig) adapter.Delivery {
	return adapter.Delivery{Target: r.binding(), VerifiedSender: true, Notice: msg.NoticeInfo}
}

func TestMailSignalFixedText(t *testing.T) {
	for _, mode := range []msg.Delivery{msg.Immediate, msg.NextTurn} {
		r := newRig(t, "2.4.0", nil)
		d := scriptSender(r)
		d.Mode = mode
		m := peerMsg(mode)
		if st, err := r.h.Inject(context.Background(), m, d); st != adapter.Mailboxed || err != nil {
			t.Fatalf("Inject %v %v", st, err)
		}
		if err := r.h.MailSignal(context.Background(), m, d); err != nil {
			t.Fatalf("MailSignal: %v", err)
		}
		ps := r.prompts()
		if len(ps) != 1 {
			t.Fatalf("%d POSTs, want 1", len(ps))
		}
		want := `{"text":"A CrossTalk message is waiting: call receive_message","delivery":"steer"}`
		if ps[0].Body != want {
			t.Fatalf("body %s, want %s", ps[0].Body, want)
		}
		if WakeText != "A CrossTalk message is waiting: call receive_message" {
			t.Fatalf("WakeText %q", WakeText)
		}
		for _, w := range []string{peerWords, "peer-a", "msg_in_1"} {
			if strings.Contains(ps[0].Body, w) {
				t.Fatalf("signal carries %q", w)
			}
		}
		if ps[0].Auth != "opencode:"+testPassword {
			t.Fatal("wrong credential")
		}
		hygiene(t, r)
	}
}

func TestMailSignalFailure(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *rig, d *adapter.Delivery)
		posts int
	}{
		{"500", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.set(func(s *service) { s.prompt.Status = 500 }) }, 1},
		{"401", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.set(func(s *service) { s.prompt.Status = 401 }) }, 1},
		{"dropped", func(_ *testing.T, r *rig, _ *adapter.Delivery) { r.svc.set(func(s *service) { s.prompt.Drop = true }) }, 1},
		{"other session", func(_ *testing.T, r *rig, _ *adapter.Delivery) {
			r.svc.set(func(s *service) { s.prompt.Session = "ses_other9" })
		}, 1},
		{"missing service.json", func(t *testing.T, r *rig, _ *adapter.Delivery) {
			r.h = NewHarness(Options{ServicePath: filepath.Join(t.TempDir(), "service.json")})
		}, 0},
		{"origin differs from binding", func(_ *testing.T, _ *rig, d *adapter.Delivery) { d.Target.HarnessEndpoint = "http://127.0.0.1:1" }, 0},
		{"refused major", func(t *testing.T, r *rig, _ *adapter.Delivery) {
			r.h = NewHarness(Options{ServicePath: writeSvc(t, t.TempDir(), r.f.srv.URL, "3.0.0", 4242)})
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "2.4.0", nil)
			d := scriptSender(r)
			tc.setup(t, r, &d)
			err := r.h.MailSignal(context.Background(), peerMsg(msg.Immediate), d)
			if err == nil {
				t.Fatal("no error for a failed signal")
			}
			clean(t, "error", err)
			if n := len(r.prompts()); n != tc.posts {
				t.Fatalf("%d POSTs, want %d", n, tc.posts)
			}
			hygiene(t, r)
		})
	}
}

func TestMailSignalDeadline(t *testing.T) {
	old := wakeTimeout
	wakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { wakeTimeout = old })
	r := newRig(t, "2.4.0", nil)
	r.svc.set(func(s *service) { s.prompt.Delay = 10 * time.Second })
	start := time.Now()
	err := r.h.MailSignal(context.Background(), peerMsg(msg.Immediate), scriptSender(r))
	if err == nil {
		t.Fatal("no error for an unacknowledged wake")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("MailSignal took %v", el)
	}
	if n := len(r.prompts()); n != 1 {
		t.Fatalf("%d POSTs, want 1", n)
	}
}
