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

func slugRegister(t *testing.T, reg *registry.Registry, ref, name string) registry.Descriptor {
	t.Helper()
	g, err := reg.Acquire(registry.Request{AgentID: identity.AgentID("codex", ref), HarnessName: name, Harness: "codex", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	g.Publish(registry.Descriptor{SessionRef: ref})
	ds, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.SessionRef == ref {
			return d
		}
	}
	t.Fatalf("%s not registered", ref)
	return registry.Descriptor{}
}

func noDiscover(context.Context) ([]Session, error) { return nil, nil }

func TestResolveTargetSlugUnique(t *testing.T) {
	reg := registry.New(t.TempDir())
	d := slugRegister(t, reg, "t1", "CrossTalk Coordinator 44")
	for _, to := range []string{"crosstalk-coordinator-44", "CROSSTALK-Coordinator-44", "crosstalk coordinator 44"} {
		tg, err := ResolveTarget(context.Background(), reg, noDiscover, nil, to)
		if err != nil || !tg.Registered || tg.Desc.AgentID != d.AgentID {
			t.Fatalf("%q: %+v %v", to, tg, err)
		}
	}
}

func TestResolveTargetSlugNative(t *testing.T) {
	reg := registry.New(t.TempDir())
	s := Session{Harness: "claude", SessionRef: "n1", Name: "Build Bot"}
	tg, err := ResolveTarget(context.Background(), reg, func(context.Context) ([]Session, error) { return []Session{s}, nil }, nil, "build-bot")
	if err != nil || len(tg.Native) != 1 || tg.Native[0].SessionRef != "n1" {
		t.Fatalf("%+v %v", tg, err)
	}
}

func TestResolveTargetSlugAmbiguousPicksNothing(t *testing.T) {
	reg := registry.New(t.TempDir())
	slugRegister(t, reg, "t1", "Build Bot")
	slugRegister(t, reg, "t2", "build-bot")
	native := Session{Harness: "claude", SessionRef: "n1", Name: "BUILD BOT"}
	tg, err := ResolveTarget(context.Background(), reg, func(context.Context) ([]Session, error) { return []Session{native}, nil }, nil, "build bot")
	if err != nil || tg.Registered || len(tg.Native) != 0 {
		t.Fatalf("picked a session: %+v %v", tg, err)
	}
	if len(tg.Candidates) != 3 {
		t.Fatalf("candidates %v", tg.Candidates)
	}
}

func TestResolveTargetExactNameBeatsSlugCollision(t *testing.T) {
	reg := registry.New(t.TempDir())
	slugRegister(t, reg, "t1", "build-bot")
	want := slugRegister(t, reg, "t2", "Build Bot")
	tg, err := ResolveTarget(context.Background(), reg, noDiscover, nil, "Build Bot")
	if err != nil || !tg.Registered || tg.Desc.AgentID != want.AgentID {
		t.Fatalf("exact lost to slug: %+v %v", tg, err)
	}
}

func TestResolveTargetAgentIDUnaffectedBySlug(t *testing.T) {
	reg := registry.New(t.TempDir())
	d := slugRegister(t, reg, "t1", "Build Bot")
	slugRegister(t, reg, "t2", "build-bot")
	tg, err := ResolveTarget(context.Background(), reg, noDiscover, nil, d.AgentID)
	if err != nil || !tg.Registered || tg.Desc.AgentID != d.AgentID || len(tg.Candidates) != 0 {
		t.Fatalf("%+v %v", tg, err)
	}
	tg, _ = ResolveTarget(context.Background(), reg, noDiscover, nil, "0123456789abcdef")
	if tg.Registered || len(tg.Candidates) != 0 || tg.Closest != "" {
		t.Fatalf("unknown id got slug treatment: %+v", tg)
	}
}

func TestResolveTargetAddressWithAtNeverSlugs(t *testing.T) {
	reg := registry.New(t.TempDir())
	slugRegister(t, reg, "t1", "Build Bot@lab")
	tg, err := ResolveTarget(context.Background(), reg, noDiscover, nil, "build-bot@lab")
	if err != nil || tg.Registered || len(tg.Candidates) != 0 {
		t.Fatalf("%+v %v", tg, err)
	}
}

func TestResolveTargetSlugSkippedWhenDiscoveryFailed(t *testing.T) {
	reg := registry.New(t.TempDir())
	slugRegister(t, reg, "t1", "Build Bot")
	failed := func(context.Context) ([]Session, error) { return nil, context.DeadlineExceeded }
	tg, err := ResolveTarget(context.Background(), reg, failed, nil, "build-bot")
	if err != nil || tg.Registered {
		t.Fatalf("slug routed on a failed discovery: %+v %v", tg, err)
	}
}

func TestResolveTargetSlugSkipsIDShapedNames(t *testing.T) {
	reg := registry.New(t.TempDir())
	s := Session{Harness: "claude", SessionRef: "n1", Name: "abcdef0123456789"}
	tg, _ := ResolveTarget(context.Background(), reg, func(context.Context) ([]Session, error) { return []Session{s}, nil }, nil, "ABCDEF0123456789")
	if len(tg.Native) != 0 {
		t.Fatalf("id-shaped name reached by slug: %+v", tg)
	}
}

func TestResolveTargetClosestName(t *testing.T) {
	reg := registry.New(t.TempDir())
	slugRegister(t, reg, "t1", "CrossTalk Coordinator 44")
	slugRegister(t, reg, "t2", "alice")
	tg, err := ResolveTarget(context.Background(), reg, noDiscover, nil, "crosstalk-coordinator-45")
	if err != nil || tg.Registered || tg.Closest != "CrossTalk Coordinator 44" {
		t.Fatalf("closest %q: %+v %v", tg.Closest, tg, err)
	}
	tg, _ = ResolveTarget(context.Background(), reg, noDiscover, nil, "zzzzzzzz")
	if tg.Closest != "" {
		t.Fatalf("far name got suggestion %q", tg.Closest)
	}
}
