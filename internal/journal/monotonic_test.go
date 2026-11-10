package journal

import "testing"

func TestAppend_DedupeStatesBackwards_ExpectIgnored(t *testing.T) {
	j, _ := open(t, t.TempDir(), Options{})
	if _, _, err := j.Accept("s", "m1", StateSubmitted); err != nil {
		t.Fatal(err)
	}
	steps := []struct{ set, want string }{
		{StateMailboxed, StateMailboxed},
		{StateUnknown, StateMailboxed},
		{StateDelivered, StateDelivered},
		{StateMailboxed, StateDelivered},
		{"accepted", StateDelivered},
		{StateConfirmed, StateConfirmed},
		{StateDelivered, StateConfirmed},
		{StateUnknown, StateConfirmed},
	}
	for _, s := range steps {
		if err := j.SetDedupeState("s", "m1", s.set); err != nil {
			t.Fatal(err)
		}
		if got, _ := j.Dedupe("s", "m1"); got != s.want {
			t.Fatalf("after %q: %q, want %q", s.set, got, s.want)
		}
	}
	rs, _ := j.Records()
	n := 0
	for _, r := range rs {
		if r.Type == TypeDedupe {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("%d dedupe records on disk, want 4 (accept, mailboxed, delivered, confirmed)", n)
	}
}
