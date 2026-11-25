package pi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFill(t *testing.T) {
	bin := `/opt/a "b"/__CROSSTALK_VERSION__/crosstalk`
	got := string(Fill(bin, "1.2.3", false, nil))
	for _, want := range []string{
		marker + "\n",
		`const BIN = "/opt/a \"b\"/__CROSSTALK_VERSION__/crosstalk";`,
		`const VERSION_TAG = "1.2.3";`,
		"\nconst noRefresh: boolean = false;\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("filled file lacks %q", want)
		}
	}
	if !strings.HasPrefix(got, marker+"\n") {
		t.Error("marker is not the first line")
	}
	for _, placeholder := range []string{`"__CROSSTALK_BIN__"`, `= "__CROSSTALK_VERSION__"`, "// __CROSSTALK_MARKER__"} {
		if strings.Contains(got, placeholder) {
			t.Errorf("filled file still has %q", placeholder)
		}
	}
}

func TestFillKeepsNoAutoRefresh(t *testing.T) {
	on := "\nconst noRefresh: boolean = true;\n"
	first := Fill("/bin/crosstalk", "1.0.0", true, nil)
	if !strings.Contains(string(first), on) {
		t.Fatal("noAutoRefresh not filled")
	}
	second := Fill("/bin/crosstalk", "1.0.1", false, first)
	if !strings.Contains(string(second), on) {
		t.Error("a regeneration dropped the no-auto-refresh choice")
	}
	plain := Fill("/bin/crosstalk", "1.0.0", false, nil)
	if strings.Contains(string(Fill("/bin/crosstalk", "1.0.1", false, plain)), on) {
		t.Error("a regeneration turned auto refresh off")
	}
}

// ADR 0037: peer text goes in only as a displayed custom message.
func TestExtensionAvoidsForbiddenCalls(t *testing.T) {
	for _, name := range []string{"sendUserMessage", "expandPromptTemplates", "nextTurn", "before_agent_start", "context_with_system", `on("context"`, "systemPrompt"} {
		if strings.Contains(source, name) {
			t.Errorf("extension source names %q", name)
		}
	}
}

func TestExtensionWithStubAPI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	filled := filepath.Join(t.TempDir(), "index.ts")
	if err := os.WriteFile(filled, Fill("/opt/cross talk/crosstalk", "1.2.3", true, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "--test", "ext/index.test.mjs")
	cmd.Env = append(os.Environ(), "CROSSTALK_PI_EXT="+filled)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
