package shim

import (
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/journal"
)

func TestPrincipalCrossTalkMatchesJournalRetention(t *testing.T) {
	if PrincipalCrossTalk != journal.PrincipalCrossTalk {
		t.Fatalf("shim notices are sent as %q but journal retention recognizes %q", PrincipalCrossTalk, journal.PrincipalCrossTalk)
	}
}
