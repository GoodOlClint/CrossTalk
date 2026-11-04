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
func testHarnesses(t *testing.T, sessionDir string) []harness.Harness {
	t.Helper()
	ch, err := claude.NewHarness(claude.HarnessOptions{SessionsDir: sessionDir, Alive: func(pid int) bool { return pid > 0 && pid < 900000 }})
	if err != nil {
		t.Fatal(err)
	}
	return []harness.Harness{codex.NewHarness(codex.HarnessOptions{}), ch}
}
