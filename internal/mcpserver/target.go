package mcpserver

import (
	"context"
	"fmt"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// Target is a resolved message target: a registered agent, or the native
// sessions the target matched (several only for an ambiguous name).
type Target struct {
	Desc       registry.Descriptor
	Registered bool
	Native     []Session
}

func isAgentID(s string) bool {
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

// ResolveTarget resolves to by precedence: registered agent_id, native
// agent_id, registered name, native name. An empty to matches nothing. A
// discovery failure that leaves an agent_id-shaped to unmatched is an error
// rather than a fall-through to name matching.
func ResolveTarget(ctx context.Context, reg *registry.Registry, discover func(context.Context) ([]Session, error), to string) (Target, error) {
	if to == "" {
		return Target{}, nil
	}
	ds, err := reg.List()
	if err != nil {
		return Target{}, fmt.Errorf("registry: %w", err)
	}
	for _, d := range ds {
		if d.AgentID == to {
			return Target{Desc: d, Registered: true}, nil
		}
	}
	ss, derr := discover(ctx)
	var byID []Session
	for _, s := range ss {
		if identity.AgentID(string(s.Harness), s.SessionRef) == to {
			byID = append(byID, s)
		}
	}
	if len(byID) > 0 {
		return Target{Native: byID}, nil
	}
	if derr != nil && isAgentID(to) {
		return Target{}, fmt.Errorf("discovery: %w", derr)
	}
	for _, d := range ds {
		if d.Name == to {
			return Target{Desc: d, Registered: true}, nil
		}
	}
	var byName []Session
	for _, s := range ss {
		if s.Name == to {
			byName = append(byName, s)
		}
	}
	return Target{Native: byName}, nil
}
