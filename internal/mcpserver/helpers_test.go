package mcpserver

import (
	"strconv"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
)

func itoa(i int) string { return strconv.Itoa(i) }

// testHarnesses is the production order: Codex, then Claude on sessionDir.
func testHarnesses(t *testing.T, sessionDir string, tune ...func(*claude.HarnessOptions)) []harness.Harness {
	t.Helper()
	o := claude.HarnessOptions{SessionsDir: sessionDir, Alive: func(pid int) bool { return pid > 0 && pid < 900000 }}
	for _, f := range tune {
		f(&o)
	}
	ch, err := claude.NewHarness(o)
	if err != nil {
		t.Fatal(err)
	}
	return []harness.Harness{codex.NewHarness(codex.HarnessOptions{}), ch}
}
