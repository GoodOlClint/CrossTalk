package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

var spike9Threads = []string{
	"01a1a300-21dc-7fe2-870a-d43593ff0992",
	"01a1a31c-60ee-7a31-acdc-b148886a7b4b",
	"01a1a31c-d5f9-7782-bbed-8d80cdcf7914",
	"01a1a31d-0313-7620-b52e-bfe533904e9a",
}

func TestAgentID(t *testing.T) {
	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	seen := map[string]string{}
	for _, h := range []string{"codex", "claude"} {
		for _, ref := range spike9Threads {
			id := AgentID(h, ref)
			sum := sha256.Sum256([]byte(h + ":" + ref))
			if want := hex.EncodeToString(sum[:])[:16]; id != want {
				t.Fatalf("AgentID(%q,%q) = %s, want %s", h, ref, id, want)
			}
			if !hex16.MatchString(id) {
				t.Fatalf("AgentID %q not 16 hex", id)
			}
			if prev, ok := seen[id]; ok {
				t.Fatalf("collision %s between %s and %s:%s", id, prev, h, ref)
			}
			seen[id] = h + ":" + ref
			if AgentID(h, ref) != id {
				t.Fatal("AgentID not deterministic")
			}
		}
	}
}

func TestAgentIDKnownVector(t *testing.T) {
	sum := sha256.Sum256([]byte("codex:01a1a300-21dc-7fe2-870a-d43593ff0992"))
	if AgentID("codex", "01a1a300-21dc-7fe2-870a-d43593ff0992") != hex.EncodeToString(sum[:8]) {
		t.Fatal("AgentID is not the first 8 bytes of sha256(harness:session_ref)")
	}
}

type takenSet map[string]bool

func (s takenSet) taken(n string) bool { return s[n] }

// Test 29 (CQ8).
func TestFallbackSpike9FourThreadsOneCwd(t *testing.T) {
	held := takenSet{}
	cwd := "/Users/x/Source/crosstalk"
	for _, ref := range spike9Threads {
		id := AgentID("codex", ref)
		name := ChooseName("", "codex", cwd, id, held.taken)
		if held[name] {
			t.Fatalf("name %q reused", name)
		}
		if want := "codex-crosstalk-" + id[:8]; name != want {
			t.Fatalf("name = %q, want %q", name, want)
		}
		held[name] = true
	}
	if len(held) != 4 {
		t.Fatalf("got %d distinct names, want 4", len(held))
	}
}

// Test 29 (CQ8), fifth agent: chosen name held → suffix extended.
func TestFallbackExtendsOnCollision(t *testing.T) {
	id := "0123456789abcdef"
	held := takenSet{"codex-proj-01234567": true}
	if got := FallbackName("codex", "/x/proj", id, held.taken); got != "codex-proj-0123456789" {
		t.Fatalf("got %q", got)
	}
	held["codex-proj-0123456789"] = true
	held["codex-proj-0123456789ab"] = true
	if got := FallbackName("codex", "/x/proj", id, held.taken); got != "codex-proj-0123456789abcd" {
		t.Fatalf("got %q", got)
	}
	for n := 8; n <= 16; n += 2 {
		held["codex-proj-"+id[:n]] = true
	}
	got := FallbackName("codex", "/x/proj", id, held.taken)
	if held[got] || !strings.HasPrefix(got, "codex-proj-"+id+"-") {
		t.Fatalf("exhausted id: got %q", got)
	}
}

func TestChooseNamePrefersFreeHarnessName(t *testing.T) {
	held := takenSet{}
	if got := ChooseName("reviewer", "claude", "/a/b", "0123456789abcdef", held.taken); got != "reviewer" {
		t.Fatalf("got %q", got)
	}
	held["reviewer"] = true
	if got := ChooseName("reviewer", "claude", "/a/b", "0123456789abcdef", held.taken); got != "claude-b-01234567" {
		t.Fatalf("got %q", got)
	}
}

func TestCwdBase(t *testing.T) {
	for in, want := range map[string]string{
		"/":             "root",
		"":              "root",
		"/a/b/":         "b",
		"/a/my repo":    "my_repo",
		"/a/x:y":        "x_y",
		"/a/ok.dir-1_2": "ok.dir-1_2",
	} {
		if got := cwdBase(in); got != want {
			t.Errorf("cwdBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidName(t *testing.T) {
	bad := []string{"", strings.Repeat("a", 65), "a@b", "a\x00b", "a\nb", "a\x7fb", "0123456789abcdef", "inbox:x", "inbox:"}
	for _, n := range bad {
		if ValidName(n) == nil {
			t.Errorf("ValidName(%q) = nil, want error", n)
		}
	}
	good := []string{"CrossTalk Reviewer 3", strings.Repeat("a", 64), "0123456789ABCDEF", "0123456789abcde", "xinbox:y"}
	for _, n := range good {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v", n, err)
		}
	}
}
