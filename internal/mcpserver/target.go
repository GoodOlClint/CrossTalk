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

// IsAgentID reports whether s has the shape of an agent_id: 16 lowercase hex characters.
func IsAgentID(s string) bool {
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

// ResolveTarget resolves to by its shape. An agent_id-shaped to matches a
// registered agent_id, then a native agent_id, and never a name. Any other to
// matches a registered name without discovery, then a native name; a single
// native match whose session is registered under another name resolves to that
// registration. An empty to matches nothing.
func ResolveTarget(ctx context.Context, reg *registry.Registry, discover func(context.Context) ([]Session, error), to string) (Target, error) {
	if to == "" {
		return Target{}, nil
	}
	ds, err := reg.List()
	if err != nil {
		return Target{}, fmt.Errorf("registry: %w", err)
	}
	id := IsAgentID(to)
	for _, d := range ds {
		if id && d.AgentID == to || !id && d.Name == to {
			return Target{Desc: d, Registered: true}, nil
		}
	}
	ss, _ := discover(ctx)
	var native []Session
	for _, s := range ss {
		if id && identity.AgentID(string(s.Harness), s.SessionRef) == to || !id && s.Name == to {
			native = append(native, s)
		}
	}
	if len(native) == 1 && !id {
		nid := identity.AgentID(string(native[0].Harness), native[0].SessionRef)
		for _, d := range ds {
			if d.AgentID == nid {
				return Target{Desc: d, Registered: true}, nil
			}
		}
	}
	return Target{Native: native}, nil
}
