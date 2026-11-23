package quiesce

import (
	"context"
	"testing"
)

func TestPauseUnlessLong(t *testing.T) {
	leave, ok := HoldLong(context.Background())
	if !ok {
		t.Fatal("HoldLong refused")
	}
	if p, long := PauseUnlessLong(); p || !long {
		t.Fatalf("paused %v long %v while long work runs", p, long)
	}
	leave()
	leave()
	if Long() != 0 || Busy() != 0 {
		t.Fatalf("long %d busy %d after leave", Long(), Busy())
	}
	p, long := PauseUnlessLong()
	if !p || long {
		t.Fatalf("paused %v long %v when idle", p, long)
	}
	Unpause()
}
