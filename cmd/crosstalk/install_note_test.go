package main

import (
	"strings"
	"testing"
)

func TestInstallEndNoteSaysRefresh(t *testing.T) {
	src := repoFile(t, "cmd/crosstalk/install.go")
	if strings.Contains(src, "keep their old CrossTalk server. Restart them") {
		t.Error("install.go still tells every running session to restart")
	}
	for _, want := range []string{"switches to the new binary on its own", "CROSSTALK_NO_AUTO_REFRESH=1", "crosstalk install --check"} {
		if !strings.Contains(src, want) {
			t.Errorf("install.go end note lacks %q", want)
		}
	}
}
