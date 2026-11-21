package main

import (
	"strings"
	"testing"
)

func TestRemoteUsageNamesAdvertiseAndBindAll(t *testing.T) {
	_, _, errs := remoteRun(t, false)
	for _, want := range []string{
		"--bind IP|all", "--advertise ADDR", "--bind all listens on every address and needs --advertise",
		"a tailnet name or IP (100.64.0.0/10) is", "recommended", "config --bind all --advertise ADDR",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("remote usage lacks %q:\n%s", want, errs)
		}
	}
}

func TestRemoteConfigFirstWriteNamesBindAll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, errs := remoteRun(t, false, "config", "--alias", "x", "--yes")
	if !strings.Contains(errs, "--bind IP or --bind all is required the first time") {
		t.Errorf("first-write refusal: %q", errs)
	}
}
