package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func labelOf(t *testing.T, h *Harness) (string, bool) {
	t.Helper()
	ss, err := h.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.SessionRef == wThread {
			return s.DeliveryMode, true
		}
	}
	return "", false
}

func TestDiscover_DeliveryMode_ExpectLabelOnlyForProvenUnhostedClaimWithSettingOn(t *testing.T) {
	e := newWakeEnv(t)
	h, sys := newWakeHarness(t, e)
	if m, ok := labelOf(t, h); !ok || m != DeliveryQueueWake {
		t.Fatalf("on + proven + unhosted: label %q present=%v, want %q", m, ok, DeliveryQueueWake)
	}

	if m, _ := labelOf(t, mustHosted(t, e)); m != "" {
		t.Errorf("hosted thread labelled %q", m)
	}

	sys.set(wThread, false, nil)
	if m, _ := labelOf(t, h); m != "" {
		t.Errorf("failed owner proof labelled %q", m)
	}
	sys.own(wThread)

	os.Remove(filepath.Join(h.wakeRoute().stateDir, SettingFile))
	if m, _ := labelOf(t, h); m != "" {
		t.Errorf("setting off labelled %q", m)
	}
}

func TestDiscover_NoRoute_ExpectNoLabel(t *testing.T) {
	h := NewHarness(HarnessOptions{Adapter: stubThreads{Adapter: adapter.NewFake(adapter.Codex)}})
	ss, _ := h.Discover(context.Background())
	for _, s := range ss {
		if s.DeliveryMode != "" {
			t.Errorf("session %+v labelled", s)
		}
	}
}

func mustHosted(t *testing.T, e *wakeEnv) *Harness {
	h, _ := newWakeHarness(t, e, wThread)
	return h
}

func TestHarnessMailSignal_InboxCopyAndRemoteSender_ExpectNoWakeRowEvenOnPullRecheck(t *testing.T) {
	for _, mod := range []func(*adapter.Delivery){
		func(d *adapter.Delivery) { d.InboxCopy = true },
		func(d *adapter.Delivery) { d.SenderRemote = true },
	} {
		e := newWakeEnv(t)
		h, _ := newWakeHarness(t, e)
		d := delivery(msg.Immediate, true)
		mod(&d)
		h.MailSignal(context.Background(), msg.Message{MessageID: "m1"}, d)
		h.MailPulled(context.Background(), adapter.Binding{SessionRef: wThread}, false, func() ([]string, error) { return []string{"m1"}, nil })
		if n := len(e.wakeRows()); n != 0 {
			t.Fatalf("%+v got %d wake rows, want 0", d, n)
		}
	}
}
