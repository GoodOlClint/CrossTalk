package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/remote"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

func TestRemoteConfigNudgesOncePerChangingWrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var got [][2]remote.Config
	prev := addressNudge
	addressNudge = func(_ string, old, next remote.Config) remote.NudgeResult {
		got = append(got, [2]remote.Config{old, next})
		return remote.NudgeResult{Sent: 3, Missed: 1}
	}
	t.Cleanup(func() { addressNudge = prev })
	run := func(args ...string) int {
		code, _, _ := remoteRun(t, false, append([]string{"config", "--yes"}, args...)...)
		return code
	}

	if run("--bind", "127.0.0.1") != 0 || len(got) != 0 {
		t.Fatalf("first configure: %d nudges, want 0", len(got))
	}
	if run("--bind", "127.0.0.1") != 0 || len(got) != 0 {
		t.Fatalf("identical rewrite: %d nudges, want 0", len(got))
	}
	if run("--alias", "other") != 0 || len(got) != 0 {
		t.Fatalf("alias-only change: %d nudges, want 0", len(got))
	}
	if run("--port", "0") == 0 || len(got) != 0 {
		t.Fatalf("refused write: %d nudges, want 0", len(got))
	}
	if code, _, _ := remoteRun(t, true, "config", "--port", "7791"); code == 0 || len(got) != 0 {
		t.Fatalf("declined write: exit %d, %d nudges, want a refusal and 0", code, len(got))
	}
	if run("--port", "7790") != 0 || len(got) != 1 {
		t.Fatalf("port change: %d nudges, want 1", len(got))
	}
	if code, out, _ := remoteRun(t, false, "config", "--yes", "--port", "7792"); code != 0 || !strings.Contains(out, "told 3 sessions") || !strings.Contains(out, "1 not told") {
		t.Errorf("config output does not report delivery: %q", out)
	}
	got = got[:1]
	if got[0][0].Port != 7745 || got[0][1].Port != 7790 {
		t.Errorf("nudge saw %d -> %d, want 7745 -> 7790", got[0][0].Port, got[0][1].Port)
	}
	if run("--advertise", "host.example") != 0 || len(got) != 2 {
		t.Fatalf("advertise change: %d nudges, want 2", len(got))
	}
	if run("--advertise", "host.example") != 0 || len(got) != 2 {
		t.Fatalf("same advertise again: %d nudges, want 2", len(got))
	}
}

func TestLocalNudgeCountsOnlyAcceptedOrQueuedAsDelivered(t *testing.T) {
	send := func(state string, err error) func(registry.Descriptor, string) error {
		return localNudgeSender(func(string, string, time.Duration) (udsingress.Ack, error) {
			return udsingress.Ack{State: state, Reason: "refused"}, err
		})
	}
	d := registry.Descriptor{UDSPath: "/s/1"}
	for _, ok := range []string{"accepted", "queued"} {
		if err := send(ok, nil)(d, "x"); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"rejected", "unreachable", "unknown", ""} {
		if err := send(bad, nil)(d, "x"); err == nil {
			t.Errorf("state %q counted as delivered", bad)
		}
	}
	if err := send("accepted", errors.New("dial")); err == nil {
		t.Error("a transport error counted as delivered")
	}
}
