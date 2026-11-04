package registry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/identity"
)

const childEnv = "REGISTRY_TEST_CHILD"

func TestMain(m *testing.M) {
	if spec := os.Getenv(childEnv); spec != "" {
		var dir, ref string
		fmt.Sscanf(spec, "%s %s", &dir, &ref)
		r := New(dir)
		g, err := r.Acquire(Request{AgentID: identity.AgentID("codex", ref), HarnessName: "reviewer", Harness: "codex", Cwd: "/w/proj"})
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(0)
		}
		if err := g.Publish(Descriptor{SessionRef: ref}); err != nil {
			fmt.Println("ERR", err)
			os.Exit(0)
		}
		fmt.Println("NAME", g.Name())
		bufio.NewReader(os.Stdin).ReadString('\n')
		g.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func req(ref, name string) Request {
	return Request{AgentID: identity.AgentID("codex", ref), HarnessName: name, Harness: "codex", Cwd: "/w/proj"}
}

func abandon(g *Registration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.nameLock != nil {
		g.nameLock.Close()
		g.nameLock = nil
	}
	g.agentLock.Close()
	g.agentLock = nil
}

func TestDescriptorSchemaKeys(t *testing.T) {
	b, _ := json.Marshal(Descriptor{})
	var m map[string]any
	json.Unmarshal(b, &m)
	want := []string{"agent_id", "name", "harness", "role", "owner", "pid", "harness_pid", "harness_started_at", "cwd", "session_ref", "a2a_url", "uds_path", "instance_id", "harness_endpoint", "harness_version", "credential_ref", "installed", "started_at", "protocol"}
	var got []string
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("keys\n got %v\nwant %v", got, want)
	}
	for _, k := range []string{"owner", "harness_pid", "harness_started_at", "harness_endpoint", "harness_version"} {
		if m[k] != nil {
			t.Errorf("%s should be null when unset, got %v", k, m[k])
		}
	}
}

func TestPublishWritesDescriptor(t *testing.T) {
	r := New(t.TempDir())
	g, err := r.Acquire(req("t1", "reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ep := "/codex.sock"
	if err := g.Publish(Descriptor{SessionRef: "t1", HarnessEndpoint: &ep, A2AURL: "http://127.0.0.1:1", CredentialRef: "/c"}); err != nil {
		t.Fatal(err)
	}
	d, err := readDescriptor(r.DescriptorPath(identity.AgentID("codex", "t1")))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "reviewer" || d.PID != os.Getpid() || d.InstanceID != g.InstanceID() || d.Role != RolePrimary || d.Owner != nil || !d.Installed || d.Protocol != 1 {
		t.Fatalf("descriptor %+v", d)
	}
	if d.UDSPath != filepath.Join(r.Dir, "sock", d.AgentID+".sock") {
		t.Fatalf("uds_path %q not agent_id-keyed", d.UDSPath)
	}
	if !r.Live(d) {
		t.Fatal("own descriptor not live")
	}
	fi, _ := os.Stat(r.DescriptorPath(d.AgentID))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(r.Dir, "agents", "*.tmp*")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

// Test 24 (C7), in-process: concurrent startup of distinct sessions under one
// harness name → exactly one holds the name, the others get distinct fallbacks.
func TestConcurrentStartupOneName(t *testing.T) {
	r := New(t.TempDir())
	const n = 8
	regs := make([]*Registration, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			g, err := r.Acquire(req(fmt.Sprintf("thread-%d", i), "reviewer"))
			if err != nil {
				t.Error(err)
				return
			}
			if err := g.Publish(Descriptor{SessionRef: fmt.Sprintf("thread-%d", i)}); err != nil {
				t.Error(err)
			}
			regs[i] = g
		})
	}
	close(start)
	wg.Wait()
	winners, names := 0, map[string]bool{}
	for i, g := range regs {
		if g == nil {
			t.Fatalf("registration %d failed", i)
		}
		defer g.Close()
		if g.Name() == "reviewer" {
			winners++
		} else if want := "codex-proj-" + identity.AgentID("codex", fmt.Sprintf("thread-%d", i))[:8]; g.Name() != want {
			t.Errorf("loser %d name %q, want %q", i, g.Name(), want)
		}
		names[g.Name()] = true
	}
	if winners != 1 || len(names) != n {
		t.Fatalf("winners=%d distinct=%d", winners, len(names))
	}
	ds, _ := r.List()
	if len(ds) != n {
		t.Fatalf("listed %d", len(ds))
	}
}

// Test 24 (C7) across real processes: flock arbitration between separate pids.
func TestConcurrentStartupOneNameProcesses(t *testing.T) {
	dir, err := os.MkdirTemp("", "reg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	const n = 4
	type child struct {
		cmd   *exec.Cmd
		stdin interface{ Close() error }
		out   *bufio.Reader
	}
	kids := make([]child, n)
	for i := range n {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s proc-thread-%d", childEnv, dir, i))
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		kids[i] = child{cmd, in, bufio.NewReader(out)}
	}
	var names []string
	for _, k := range kids {
		line, _ := k.out.ReadString('\n')
		names = append(names, strings.TrimSpace(line))
	}
	r := New(dir)
	ds, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kids {
		k.stdin.Close()
		k.cmd.Wait()
	}
	winners, distinct := 0, map[string]bool{}
	for _, nm := range names {
		if !strings.HasPrefix(nm, "NAME ") {
			t.Fatalf("child: %q", nm)
		}
		nm = strings.TrimPrefix(nm, "NAME ")
		if nm == "reviewer" {
			winners++
		}
		distinct[nm] = true
	}
	if winners != 1 || len(distinct) != n {
		t.Fatalf("names %v: winners=%d", names, winners)
	}
	if len(ds) != n {
		t.Fatalf("listed %d live descriptors while children ran, want %d", len(ds), n)
	}
	after, _ := r.List()
	if len(after) != 0 {
		t.Fatalf("descriptors left after clean exit: %d", len(after))
	}
}

func TestSameAgentTwiceHeld(t *testing.T) {
	r := New(t.TempDir())
	var wg sync.WaitGroup
	results := make([]error, 6)
	regs := make([]*Registration, 6)
	start := make(chan struct{})
	for i := range results {
		wg.Go(func() {
			<-start
			regs[i], results[i] = r.Acquire(req("t1", "reviewer"))
		})
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, err := range results {
		switch {
		case err == nil:
			ok++
			defer regs[i].Close()
		case !errors.Is(err, ErrAgentHeld):
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d registrations of one agent_id", ok)
	}
}

// Tests 15 and 24: a stale cleanup by the previous instance never removes
// the replacement's descriptor.
func TestStaleCleanupVsReplacement(t *testing.T) {
	r := New(t.TempDir())
	a, err := r.Acquire(req("t1", "reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	a.Publish(Descriptor{SessionRef: "t1"})
	if _, err := r.Acquire(req("t1", "reviewer")); !errors.Is(err, ErrAgentHeld) {
		t.Fatalf("second binding while first holds: %v", err)
	}
	staleInstance := a.InstanceID()
	abandon(a)

	b, err := r.Acquire(req("t1", "reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Name() != "reviewer" {
		t.Fatalf("replacement name %q", b.Name())
	}

	agentID := b.Descriptor().AgentID
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				if removed, _ := r.RemoveIfOwned(agentID, staleInstance); removed {
					t.Error("stale cleanup removed a descriptor")
				}
			}
		}
	})
	for range 50 {
		if err := b.Publish(Descriptor{SessionRef: "t1"}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := readDescriptor(r.DescriptorPath(agentID))
	if err != nil || d.InstanceID != b.InstanceID() {
		t.Fatalf("replacement descriptor lost: %+v %v", d, err)
	}
	b.Close()
	if _, err := os.Stat(r.DescriptorPath(agentID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("own Close did not remove own descriptor")
	}
}

func TestCloseLeavesForeignInstance(t *testing.T) {
	r := New(t.TempDir())
	g, _ := r.Acquire(req("t1", ""))
	g.Publish(Descriptor{SessionRef: "t1"})
	d := g.Descriptor()
	d.InstanceID = "someone-else"
	writeAtomic(r.DescriptorPath(d.AgentID), d)
	g.Close()
	if _, err := os.Stat(r.DescriptorPath(d.AgentID)); err != nil {
		t.Fatal("Close removed a descriptor with another instance_id")
	}
}

// CQ9: rename moves the alias only.
func TestRenameKeepsAgentID(t *testing.T) {
	r := New(t.TempDir())
	g, _ := r.Acquire(req("t1", "old"))
	defer g.Close()
	g.Publish(Descriptor{SessionRef: "t1"})
	before := g.Descriptor()

	other, _ := r.Acquire(req("t2", "taken"))
	defer other.Close()
	if err := g.Rename("taken"); !errors.Is(err, ErrNameHeld) {
		t.Fatalf("rename to held name: %v", err)
	}
	if g.Name() != "old" {
		t.Fatal("failed rename changed the name")
	}

	if err := g.Rename("new"); err != nil {
		t.Fatal(err)
	}
	after, _ := readDescriptor(r.DescriptorPath(before.AgentID))
	if after.Name != "new" || after.AgentID != before.AgentID || after.InstanceID != before.InstanceID || after.UDSPath != before.UDSPath {
		t.Fatalf("before %+v\nafter %+v", before, after)
	}
	if d, ok, _ := r.Lookup("new"); !ok || d.AgentID != before.AgentID {
		t.Fatal("new alias does not resolve")
	}
	if _, ok, _ := r.Lookup("old"); ok {
		t.Fatal("old alias still resolves")
	}
	h, err := r.Acquire(req("t3", "old"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Name() != "old" {
		t.Fatalf("old alias not released: got %q", h.Name())
	}
	probe, err := r.Acquire(req("t4", "new"))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if probe.Name() == "new" {
		t.Fatal("new alias not held after rename")
	}
}

func TestListPrunesDeadAndReusedPids(t *testing.T) {
	r := New(t.TempDir())
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := Descriptor{AgentID: "dead000000000000", Name: "dead", InstanceID: "i1", PID: cmd.Process.Pid, StartedAt: time.Now()}
	reused := Descriptor{AgentID: "reused0000000000", Name: "reused", InstanceID: "i2", PID: os.Getpid(), StartedAt: time.Unix(1, 0)}
	heldStale := Descriptor{AgentID: "held000000000000", Name: "held", InstanceID: "i3", PID: cmd.Process.Pid, StartedAt: time.Now()}
	for _, d := range []Descriptor{dead, reused, heldStale} {
		writeAtomic(r.DescriptorPath(d.AgentID), d)
	}
	lock, ok, err := tryLock(r.agentLockPath(heldStale.AgentID))
	if err != nil || !ok {
		t.Fatal("could not hold lock")
	}
	defer lock.Close()

	live, _ := r.Acquire(req("t1", "live"))
	defer live.Close()
	live.Publish(Descriptor{SessionRef: "t1"})

	ds, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Name != "live" {
		t.Fatalf("listed %+v", ds)
	}
	for _, d := range []Descriptor{dead, reused} {
		if _, err := os.Stat(r.DescriptorPath(d.AgentID)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s not pruned", d.Name)
		}
	}
	if _, err := os.Stat(r.DescriptorPath(heldStale.AgentID)); err != nil {
		t.Error("stale descriptor pruned while its agent lock is held (replacement mid-publish)")
	}
}

func TestNameLockPathConfined(t *testing.T) {
	r := New("/s")
	for _, n := range []string{"../evil", "a/b", ".hidden", "..", "x y"} {
		p := r.nameLockPath(n)
		if filepath.Dir(p) != "/s/names" {
			t.Errorf("%q → %q escapes names/", n, p)
		}
	}
	if r.nameLockPath("a/b") == r.nameLockPath("a_b") {
		t.Error("distinct names share a lock")
	}
}

func TestResolveByAgentIDNameAndEmpty(t *testing.T) {
	r := New(t.TempDir())
	a, _ := r.Acquire(req("t1", "alpha"))
	defer a.Close()
	a.Publish(Descriptor{SessionRef: "t1"})
	b, _ := r.Acquire(req("t2", ""))
	defer b.Close()
	b.Publish(Descriptor{SessionRef: "t2"})
	ida, idb := a.Descriptor().AgentID, b.Descriptor().AgentID
	if d, ok, _ := r.Resolve(ida); !ok || d.AgentID != ida {
		t.Fatal("agent_id does not resolve")
	}
	if d, ok, _ := r.Resolve(idb); !ok || d.AgentID != idb {
		t.Fatal("empty-named agent does not resolve by agent_id")
	}
	if d, ok, _ := r.Resolve("alpha"); !ok || d.AgentID != ida {
		t.Fatal("name does not resolve")
	}
	if _, ok, _ := r.Resolve(""); ok {
		t.Fatal("empty target resolves")
	}
	if _, ok, _ := r.Lookup(""); ok {
		t.Fatal("empty name resolves")
	}
}
