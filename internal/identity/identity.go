package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
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

const (
	MaxNameBytes = 64
	// InboxPrefix starts the address form of a role inbox and is reserved,
	// in any case, in session names.
	InboxPrefix = "inbox:"
)

// CutInbox returns the inbox name after an inbox: prefix, matched in any case.
func CutInbox(s string) (string, bool) {
	if len(s) >= len(InboxPrefix) && strings.EqualFold(s[:len(InboxPrefix)], InboxPrefix) {
		return s[len(InboxPrefix):], true
	}
	return "", false
}

// ValidName reports why name cannot be a session alias: empty, over
// MaxNameBytes, surrounding space (HTTP trims it from the A2A sender header),
// a control character, "@" (separates a paired-host alias), the
// shape of an agent_id (never matched as a name), or the reserved inbox: prefix in any case.
func ValidName(name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case len(name) > MaxNameBytes:
		return errors.New("name is longer than 64 bytes")
	case hasInboxPrefix(name):
		return errors.New("name starts with the reserved prefix inbox:")
	case strings.TrimSpace(name) != name:
		return errors.New("name starts or ends with a space")
	case strings.Contains(name, "@"):
		return errors.New("name contains @")
	case isHex16(name):
		return errors.New("name has the shape of an agent_id")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("name contains a control character")
		}
	}
	return nil
}

func isHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func hasInboxPrefix(s string) bool {
	_, ok := CutInbox(s)
	return ok
}
