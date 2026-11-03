package shim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/procstart"
)

func (s *Shim) alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if s.opt.Alive != nil {
		return s.opt.Alive(pid)
	}
	_, err := procstart.StartTime(pid)
	return err == nil
}

// claudeSessions reads every live session file; it never consults them for
// sender identity (§3).
func (s *Shim) claudeSessions() []claude.Session {
	paths, _ := filepath.Glob(filepath.Join(s.opt.SessionsDir, "*.json"))
	var out []claude.Session
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cs claude.Session
		if json.Unmarshal(b, &cs) != nil || cs.SessionID == "" || filepath.Base(p) != itoa(cs.PID)+".json" || !s.alive(cs.PID) {
			continue
		}
		out = append(out, cs)
	}
	return out
}

func socketExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// codexThreads queries the managed daemon plus every harness_endpoint a
// Codex descriptor records (§3 CQ10). An absent daemon yields nothing.
func (s *Shim) codexThreads(ctx context.Context) []codex.LoadedThread {
	var extra []string
	if ds, err := s.reg.List(); err == nil {
		for _, d := range ds {
			if d.Harness == string(adapter.Codex) && d.HarnessEndpoint != nil && *d.HarnessEndpoint != "" {
				extra = append(extra, *d.HarnessEndpoint)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, s.opt.DiscoveryTimeout)
	defer cancel()
	return s.codex.LoadedThreads(ctx, extra...)
}

// Discover lists every live Claude session and every loaded Codex thread.
// mcpserver merges the descriptor directory on top and sets crosstalk,
// role and owner from it.
func (s *Shim) Discover(ctx context.Context) ([]mcpserver.Session, error) {
	var out []mcpserver.Session
	for _, cs := range s.claudeSessions() {
		out = append(out, mcpserver.Session{Harness: adapter.Claude, SessionRef: cs.SessionID, Name: cs.Name, Cwd: cs.Cwd, Status: cs.Status, Reachable: socketExists(cs.MessagingSocketPath)})
	}
	for _, t := range s.codexThreads(ctx) {
		out = append(out, mcpserver.Session{Harness: adapter.Codex, SessionRef: t.ID, Name: t.Name, Cwd: t.Cwd, Status: t.Status, Reachable: t.Session.Reachable})
	}
	return out, nil
}

// nativeTarget resolves a name with no descriptor to exactly one harness
// session, injected through this agent's own adapters (§3: inform only).
func (st *AgentState) nativeTarget(ctx context.Context, name string) (adapter.Binding, adapter.Adapter, adapter.State, string) {
	s := st.shim
	type cand struct {
		h   adapter.Harness
		ref string
		cs  claude.Session
	}
	var cs []cand
	for _, c := range s.claudeSessions() {
		if c.Name == name {
			cs = append(cs, cand{h: adapter.Claude, ref: c.SessionID, cs: c})
		}
	}
	for _, t := range s.codexThreads(ctx) {
		if t.Name == name {
			cs = append(cs, cand{h: adapter.Codex, ref: t.ID})
		}
	}
	switch len(cs) {
	case 0:
		return adapter.Binding{}, nil, adapter.Unknown, ReasonNoSuchAgent
	case 1:
	default:
		return adapter.Binding{}, nil, adapter.Rejected, ReasonAmbiguousName
	}
	c := cs[0]
	if c.h == st.binding.Harness && c.ref == st.binding.SessionRef {
		return adapter.Binding{}, nil, adapter.Rejected, ReasonSelf
	}
	if c.h == adapter.Claude {
		return c.cs.Binding(), st.claude, "", ""
	}
	b, err := s.codex.Register(ctx, c.ref)
	if err != nil {
		return adapter.Binding{}, nil, adapter.Unreachable, err.Error()
	}
	return b, s.codex, "", ""
}
