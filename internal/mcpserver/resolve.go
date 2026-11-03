package mcpserver

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
)

// caller is the session a single tools/call came from (§3).
type caller struct {
	harness    adapter.Harness
	sessionRef string
	sessionID  string
	name       string
	cwd        string
	harnessPID int
}

func (c caller) bound() bool { return c.harness != "" }

// claudeSession is the subset of ~/.claude/sessions/<pid>.json the shim reads.
type claudeSession struct {
	PID                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	Name                string `json:"name"`
	Cwd                 string `json:"cwd"`
	Status              string `json:"status"`
	MessagingSocketPath string `json:"messagingSocketPath"`
}

func readClaudeSession(path string) (claudeSession, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return claudeSession{}, err
	}
	var s claudeSession
	if err := json.Unmarshal(b, &s); err != nil {
		return claudeSession{}, err
	}
	return s, nil
}

// resolveCaller applies the §3 order: _meta.threadId → Codex; else the parent
// pid's Claude session record → Claude; else unbound. _meta.sessionId is never
// an identity on its own.
func resolveCaller(meta map[string]any, sessionDir string, ppid int) (caller, error) {
	if tid, ok := meta["threadId"].(string); ok && tid != "" {
		c := caller{harness: adapter.Codex, sessionRef: tid}
		if sid, ok := meta["sessionId"].(string); ok {
			c.sessionID = sid
		}
		return c, nil
	}
	if ppid <= 1 || sessionDir == "" {
		return caller{}, nil
	}
	s, err := readClaudeSession(filepath.Join(sessionDir, strconv.Itoa(ppid)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return caller{}, nil
	}
	if err != nil {
		return caller{}, err
	}
	if s.SessionID == "" {
		return caller{}, nil
	}
	return caller{harness: adapter.Claude, sessionRef: s.SessionID, name: s.Name, cwd: s.Cwd, harnessPID: ppid}, nil
}
