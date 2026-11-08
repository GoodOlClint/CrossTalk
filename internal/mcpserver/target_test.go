package mcpserver

import (
	"context"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func TestResolveTargetByRegisteredNameSkipsDiscovery(t *testing.T) {
	reg := registry.New(t.TempDir())
	g, err := reg.Acquire(registry.Request{AgentID: identity.AgentID("codex", "t1"), HarnessName: "bob", Harness: "codex", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Publish(registry.Descriptor{SessionRef: "t1"})
	calls := 0
	discover := func(context.Context) ([]Session, error) { calls++; return nil, nil }
	tg, err := ResolveTarget(context.Background(), reg, discover, nil, "bob")
	if err != nil || !tg.Registered {
		t.Fatalf("not resolved: %+v %v", tg, err)
	}
	if calls != 0 {
		t.Fatalf("discover called %d times", calls)
	}
}

func TestResolveTargetAgentIDShapeNeverMatchesName(t *testing.T) {
	reg := registry.New(t.TempDir())
	const id = "0123456789abcdef"
	g, err := reg.Acquire(registry.Request{AgentID: identity.AgentID("codex", "t1"), HarnessName: id, Harness: "codex", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Publish(registry.Descriptor{SessionRef: "t1"})
	tg, err := ResolveTarget(context.Background(), reg, func(context.Context) ([]Session, error) { return nil, nil }, nil, id)
	if err != nil || tg.Registered || len(tg.Native) != 0 {
		t.Fatalf("name matched an agent_id-shaped target: %+v %v", tg, err)
	}
}

func TestSplitAddress(t *testing.T) {
	for in, want := range map[string][3]any{
		"bob@lab":              {"bob", "lab", true},
		"a@b@lab":              {"a@b", "lab", true},
		"bob":                  {"", "", false},
		"@lab":                 {"", "", false},
		"bob@":                 {"", "", false},
		"0123456789abcdef@lab": {"0123456789abcdef", "lab", true},
	} {
		ref, alias, ok := SplitAddress(in)
		if ref != want[0] || alias != want[1] || ok != want[2] {
			t.Errorf("SplitAddress(%q) = %q %q %v", in, ref, alias, ok)
		}
	}
}
