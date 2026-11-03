package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	fallbackHex = 8
	fallbackExt = 2
)

func AgentID(harness, sessionRef string) string {
	sum := sha256.Sum256([]byte(harness + ":" + sessionRef))
	return hex.EncodeToString(sum[:])[:16]
}

func ChooseName(harnessName, harness, cwd, agentID string, taken func(string) bool) string {
	if harnessName != "" && !taken(harnessName) {
		return harnessName
	}
	return FallbackName(harness, cwd, agentID, taken)
}

// FallbackName is <harness>-<cwd basename>-<agent_id prefix>, starting at 8 hex
// and extending 2 hex at a time while taken reports the name as held. If the
// whole agent_id is taken, a numeric suffix is appended so the result never
// equals a taken name.
func FallbackName(harness, cwd, agentID string, taken func(string) bool) string {
	base := harness + "-" + cwdBase(cwd) + "-"
	n := min(fallbackHex, len(agentID))
	for {
		name := base + agentID[:n]
		if !taken(name) {
			return name
		}
		if n >= len(agentID) {
			break
		}
		n = min(n+fallbackExt, len(agentID))
	}
	full := base + agentID
	for i := 2; ; i++ {
		name := full + "-" + strconv.Itoa(i)
		if !taken(name) {
			return name
		}
	}
}

func cwdBase(cwd string) string {
	b := filepath.Base(filepath.Clean(cwd))
	if b == "" || b == "." || b == string(filepath.Separator) {
		return "root"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, b)
}
