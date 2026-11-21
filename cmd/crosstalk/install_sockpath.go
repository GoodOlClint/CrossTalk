package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

// installSockPathCheck is K12, warn-only: an agent's unix socket path is
// 16 hex digits under <state>/sock, and a path over the platform limit makes
// every registration fail, leaving each session crosstalk:false.
func installSockPathCheck(home string) harness.CheckResult {
	p := filepath.Join(home, ".local", "state", "crosstalk", "sock", strings.Repeat("0", 16)+".sock")
	r := harness.CheckResult{ID: "K12", WarnOnly: true}
	if len(p) > udsingress.MaxSockPath {
		r.Detail = fmt.Sprintf("agent socket path %s is %d bytes, over the %d-byte limit: unix socket path too long, so sessions cannot register; shorten HOME", p, len(p), udsingress.MaxSockPath)
		return r
	}
	r.OK, r.Detail = true, fmt.Sprintf("agent socket paths fit the %d-byte limit", udsingress.MaxSockPath)
	return r
}
