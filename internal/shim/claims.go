package shim

import (
	"context"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func rejected(reason string) *mcpserver.Error {
	return &mcpserver.Error{Code: mcpserver.CodeRejected, Reason: reason}
}

// Claim and Release reach a target served by another shim process; mcpserver
// handles targets in this process itself.
func (s *Shim) Claim(ctx context.Context, owner *mcpserver.Agent, target registry.Descriptor) error {
	return s.setClaim(ctx, owner, target, true)
}

func (s *Shim) Release(ctx context.Context, owner *mcpserver.Agent, target registry.Descriptor) error {
	return s.setClaim(ctx, owner, target, false)
}

func (s *Shim) setClaim(ctx context.Context, owner *mcpserver.Agent, target registry.Descriptor, claimed bool) error {
	if target.AgentID == owner.ID() {
		return rejected(ReasonSelf)
	}
	ct, ok := s.opt.Transport.(ClaimTransport)
	if !ok {
		return &mcpserver.Error{Code: string(adapter.Unreachable), Reason: ReasonNoTransport}
	}
	return ct.SetClaim(ctx, owner.Descriptor(), target, claimed)
}

// ApplyClaim is the receiving side of a claim or release on targetID, served
// here (§7): the owner must be a live registered primary other than the
// target; the target's own journal decides already-claimed and not-owner.
func (s *Shim) ApplyClaim(_ context.Context, owner registry.Descriptor, targetID string, claimed bool) error {
	st := s.Agent(targetID)
	if st == nil {
		return rejected(ReasonNotServedHere)
	}
	if owner.AgentID == targetID {
		return rejected(ReasonSelf)
	}
	live, ok := s.descriptorByID(owner.AgentID)
	if !ok {
		return rejected(ReasonNoSuchAgent)
	}
	if !claimed {
		return st.agent.ApplyRelease(live.AgentID)
	}
	if live.Role != registry.RolePrimary {
		return rejected(ReasonNotPrimary)
	}
	return st.agent.ApplyClaim(live.AgentID, live.Name)
}
