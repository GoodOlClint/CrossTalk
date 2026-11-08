package journal

import "testing"

func TestClaimForScopesAreSeparate(t *testing.T) {
	j, _, err := Open(t.TempDir(), "a1a1a1a1a1a1a1a1", Binding{InstanceID: "i", SessionRef: "s"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	const id = "01a1b83a-0000-7000-8000-000000000001"
	for _, scope := range []string{"", "remote:aa/b1", "remote:bb/b1"} {
		if ok, err := j.ClaimFor(scope, id); !ok || err != nil {
			t.Fatalf("claim in scope %q: %v %v", scope, ok, err)
		}
		if ok, _ := j.ClaimFor(scope, id); ok {
			t.Fatalf("scope %q claimed twice", scope)
		}
	}
	if ok, _ := j.CancelFor("remote:cc/b1", id); !ok {
		t.Fatal("cancel in an unclaimed scope refused")
	}
	if l, ok := j.DispatchState(id); !ok || l.Sender != "" {
		t.Fatalf("local scope %+v %v", l, ok)
	}
}

func TestAcceptRecordRefusesAnotherSendersID(t *testing.T) {
	j, _, err := Open(t.TempDir(), "a1a1a1a1a1a1a1a1", Binding{InstanceID: "i", SessionRef: "s"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, dup, err := j.AcceptRecord(Record{Sender: "x", MessageID: "m1", State: StateSubmitted}); dup || err != nil {
		t.Fatal(dup, err)
	}
	if _, _, err := j.AcceptRecord(Record{Sender: "y", MessageID: "m1", State: StateSubmitted}); err != ErrIDInUse {
		t.Fatalf("err %v", err)
	}
	if _, dup, _ := j.AcceptRecord(Record{Sender: "x", MessageID: "m1", State: StateSubmitted}); !dup {
		t.Fatal("same sender not a duplicate")
	}
}
