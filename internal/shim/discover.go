package shim

import (
	"context"
	"errors"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

type found struct {
	h harness.Harness
	s harness.Session
}

// sessions unions every harness's Discover, each bounded by
// DiscoveryTimeout; a harness that fails contributes what it returned.
func (s *Shim) sessions(ctx context.Context) ([]found, error) {
	var out []found
	var errs []error
	for _, h := range s.opt.Harnesses {
		hctx, cancel := context.WithTimeout(ctx, s.opt.DiscoveryTimeout)
		ss, err := h.Discover(hctx)
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
		for _, x := range ss {
			out = append(out, found{h: h, s: x})
		}
	}
	return out, errors.Join(errs...)
}

// Discover lists every live session of every harness. mcpserver merges the
// descriptor directory on top and sets crosstalk, role and owner from it.
func (s *Shim) Discover(ctx context.Context) ([]mcpserver.Session, error) {
	fs, err := s.sessions(ctx)
	var out []mcpserver.Session
	for _, f := range fs {
		out = append(out, f.s)
	}
	return out, err
}

// nativeTarget resolves a name or agent_id with no descriptor to exactly one harness
// session, injected through this agent's own injectors (§3: inform only).
func (st *AgentState) nativeTarget(ctx context.Context, name string) (adapter.Binding, adapter.Adapter, adapter.State, string) {
	c, state, reason := st.nativeMatch(ctx, name)
	if reason != "" {
		return adapter.Binding{}, nil, state, reason
	}
	b, err := c.h.Register(ctx, c.s.SessionRef)
	if err != nil {
		return adapter.Binding{}, nil, adapter.Unreachable, err.Error()
	}
	return b, st.injector(c.h), "", ""
}

// nativeMatch only matches name against discovered sessions; it binds and
// contacts nothing.
func (st *AgentState) nativeMatch(ctx context.Context, name string) (found, adapter.State, string) {
	fs, _ := st.shim.sessions(ctx)
	var cs []found
	if name != "" {
		for _, f := range fs {
			if identity.AgentID(string(f.s.Harness), f.s.SessionRef) == name {
				cs = append(cs, f)
			}
		}
		if len(cs) == 0 {
			for _, f := range fs {
				if f.s.Name == name {
					cs = append(cs, f)
				}
			}
		}
	}
	switch len(cs) {
	case 0:
		return found{}, adapter.Unknown, ReasonNoSuchAgent
	case 1:
	default:
		return found{}, adapter.Rejected, ReasonAmbiguousName
	}
	c := cs[0]
	if c.s.Harness == st.binding.Harness && c.s.SessionRef == st.binding.SessionRef {
		return found{}, adapter.Rejected, ReasonSelf
	}
	return c, "", ""
}
