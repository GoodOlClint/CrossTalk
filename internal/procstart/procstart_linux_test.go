package procstart

import (
	"os"
	"testing"
	"time"
)

func TestTokenIgnoresBootTimeStep(t *testing.T) {
	a, err := Token(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := StartTime(os.Getpid())
	bootTime = bootTime.Add(5 * time.Second)
	defer func() { bootTime = bootTime.Add(-5 * time.Second) }()
	s2, _ := StartTime(os.Getpid())
	b, _ := Token(os.Getpid())
	if s1.Equal(s2) {
		t.Fatal("test did not move the derived start time")
	}
	if a != b {
		t.Fatalf("token changed with the boot time: %s vs %s", a, b)
	}
}

func TestTokenDiffersAcrossBoots(t *testing.T) {
	if token("boot-a", 42) == token("boot-b", 42) {
		t.Fatal("identical ticks on two boots compare equal")
	}
}
