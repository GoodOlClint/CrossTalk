package msg

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func valid() Message {
	return Message{
		MessageID: NewUUIDv7(),
		ContextID: "ctx-1",
		From:      "alice",
		To:        "bob",
		Kind:      Inform,
		Delivery:  Immediate,
		HopChain:  []string{Hop("a")},
		SentAt:    time.Unix(1_700_000_000, 0).UTC(),
		Parts:     []Part{{Type: "text", Text: "hi"}},
	}
}

func TestUUIDv7Format(t *testing.T) {
	for range 1000 {
		u := NewUUIDv7()
		if !IsUUIDv7(u) {
			t.Fatalf("%q is not a uuidv7", u)
		}
	}
	for _, bad := range []string{"", "01a1a300-21dc-4fe2-870a-d43593ff0992", "01a1a300-21dc-7fe2-c70a-d43593ff0992", "01A1A300-21DC-7FE2-870A-D43593FF0992"} {
		if IsUUIDv7(bad) {
			t.Errorf("IsUUIDv7(%q) = true", bad)
		}
	}
	if !IsUUIDv7("01a1a300-21dc-7fe2-870a-d43593ff0992") {
		t.Error("spike-9 thread id rejected")
	}
}

func TestUUIDv7TimestampAndOrder(t *testing.T) {
	before := time.Now().UnixMilli()
	var ids []string
	for range 5000 {
		ids = append(ids, NewUUIDv7())
	}
	after := time.Now().UnixMilli()
	if !slices.IsSorted(ids) {
		t.Fatal("ids from one process are not monotonic")
	}
	ts := func(u string) int64 {
		h := strings.ReplaceAll(u, "-", "")[:12]
		var v int64
		for _, c := range h {
			v = v<<4 | int64(strings.IndexRune("0123456789abcdef", c))
		}
		return v
	}
	if got := ts(ids[0]); got < before || got > after+1 {
		t.Fatalf("timestamp %d outside [%d,%d]", got, before, after)
	}
}

func TestUUIDv7Concurrent(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 2000 {
				u := NewUUIDv7()
				mu.Lock()
				if seen[u] {
					t.Errorf("duplicate %s", u)
				}
				seen[u] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
}

func TestUUIDv7CounterOverflowAdvancesTime(t *testing.T) {
	old := nowMilli
	defer func() { nowMilli = old }()
	nowMilli = func() int64 { return 1 }
	uuidMu.Lock()
	uuidLast = 0
	uuidMu.Unlock()
	var prev string
	for range 0x1000 + 10 {
		u := NewUUIDv7()
		if u <= prev {
			t.Fatalf("not increasing: %s after %s", u, prev)
		}
		prev = u
	}
}

func TestValidate(t *testing.T) {
	ok := valid()
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid message rejected: %v", err)
	}
	bad := func(s string) *string { return &s }
	cases := []struct {
		name string
		mut  func(*Message)
		want error
	}{
		{"message_id", func(m *Message) { m.MessageID = "x" }, ErrInvalidField},
		{"in_reply_to", func(m *Message) { m.InReplyTo = bad("nope") }, ErrInvalidField},
		{"context_id", func(m *Message) { m.ContextID = "" }, ErrInvalidField},
		{"task_id", func(m *Message) { m.TaskID = bad("") }, ErrInvalidField},
		{"from", func(m *Message) { m.From = "" }, ErrInvalidField},
		{"to", func(m *Message) { m.To = "" }, ErrInvalidField},
		{"kind control", func(m *Message) { m.Kind = "control" }, ErrUnknownKind},
		{"kind empty", func(m *Message) { m.Kind = "" }, ErrUnknownKind},
		{"delivery", func(m *Message) { m.Delivery = "later" }, ErrInvalidField},
		{"deadline on inform", func(m *Message) { m.DeadlineSeconds = 5 }, ErrInvalidField},
		{"deadline negative", func(m *Message) { m.Kind = Ask; m.DeadlineSeconds = -1 }, ErrInvalidField},
		{"sent_at", func(m *Message) { m.SentAt = time.Time{} }, ErrInvalidField},
		{"parts empty", func(m *Message) { m.Parts = nil }, ErrInvalidField},
		{"part type", func(m *Message) { m.Parts = []Part{{Type: "file"}} }, ErrInvalidField},
		{"hop format", func(m *Message) { m.HopChain = []string{"abc"} }, ErrInvalidField},
		{"hop repeat", func(m *Message) { m.HopChain = []string{Hop("a"), Hop("a")} }, ErrHopRepeat},
	}
	for _, c := range cases {
		m := valid()
		c.mut(&m)
		if err := m.Validate(); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	m := valid()
	m.Kind, m.DeadlineSeconds = Ask, 30
	m.InReplyTo = bad(NewUUIDv7())
	m.TaskID = bad("t1")
	if err := m.Validate(); err != nil {
		t.Fatalf("ask with deadline rejected: %v", err)
	}
}

func TestHop(t *testing.T) {
	h := Hop("agent")
	if len(h) != 24 || !hop24RE.MatchString(h) {
		t.Fatalf("Hop = %q", h)
	}
}

func TestHopChainOverflow(t *testing.T) {
	var chain []string
	var err error
	for i := range MaxHops {
		if chain, err = AppendHop(chain, Hop(string(rune('A'+i)))); err != nil {
			t.Fatalf("hop %d: %v", i, err)
		}
	}
	if err := CheckHopChain(chain); err != nil {
		t.Fatalf("32 hops rejected: %v", err)
	}
	if _, err := AppendHop(chain, Hop("overflow")); !errors.Is(err, ErrHopOverflow) {
		t.Fatalf("33rd hop: %v", err)
	}
	if err := CheckHopChain(append(slices.Clone(chain), Hop("x"))); !errors.Is(err, ErrHopOverflow) {
		t.Fatalf("33 entries: %v", err)
	}
}

// Test 7 core: A→B→A is rejected at the repeat.
func TestHopChainLoop(t *testing.T) {
	a, b := Hop("A"), Hop("B")
	chain, _ := AppendHop(nil, a)
	chain, _ = AppendHop(chain, b)
	if _, err := AppendHop(chain, a); !errors.Is(err, ErrHopRepeat) {
		t.Fatalf("loop not detected: %v", err)
	}
}

func TestAppendHopDoesNotAlias(t *testing.T) {
	base := make([]string, 1, 4)
	base[0] = Hop("a")
	x, _ := AppendHop(base, Hop("b"))
	y, _ := AppendHop(base, Hop("c"))
	if x[1] == y[1] {
		t.Fatal("AppendHop aliased the input slice")
	}
}

func TestJSONShape(t *testing.T) {
	m := valid()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, k := range []string{"message_id", "in_reply_to", "context_id", "task_id", "from", "to", "kind", "delivery", "hop_chain", "sent_at", "parts"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing field %q in %s", k, raw)
		}
	}
	if _, ok := got["deadline_seconds"]; ok {
		t.Error("deadline_seconds present on inform")
	}
	var back Message
	if err := json.Unmarshal(raw, &back); err != nil || back.Validate() != nil {
		t.Fatalf("round trip: %v", err)
	}
}
