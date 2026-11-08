package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type claimOut struct {
	Name   string  `json:"name"`
	Owner  string  `json:"owner,omitempty"`
	Inform *Result `json:"inform,omitempty"`
	// InformError is set when the claim is recorded but the target could not be told.
	InformError string `json:"inform_error,omitempty"`
}

func (s *Server) addClaimTools() {
	boundTool(s, &mcp.Tool{
		Name:        "claim",
		Description: "Become the owner of an unclaimed registered agent, named by name or agent_id; it becomes your subagent. Returns {name, owner}.",
		InputSchema: schemaFor[nameIn](nil),
	}, func(ctx context.Context, a *Agent, in nameIn) (any, error) {
		return s.claim(ctx, a, in.Name)
	})
	boundTool(s, &mcp.Tool{
		Name:        "release",
		Description: "End your claim on an agent you own, named by name or agent_id; it returns to primary. Returns {name}.",
		InputSchema: schemaFor[nameIn](nil),
	}, func(ctx context.Context, a *Agent, in nameIn) (any, error) {
		return s.release(ctx, a, in.Name)
	})
}

func (s *Server) claimTarget(ctx context.Context, a *Agent, name string) (registry.Descriptor, error) {
	r, err := ResolveTarget(ctx, s.reg, s.backend.Discover, aliasChecker(s.backend), name)
	if err != nil {
		return registry.Descriptor{}, err
	}
	if r.Remote != nil && !r.Registered {
		return registry.Descriptor{}, &Error{Code: CodeRejected, Reason: "remote_kind", Hint: HintRemoteKind}
	}
	if !r.Registered {
		return registry.Descriptor{}, rejected("no_such_agent")
	}
	t := r.Desc
	if t.AgentID == a.id {
		return registry.Descriptor{}, rejected("self")
	}
	return t, nil
}

func (s *Server) claim(ctx context.Context, a *Agent, name string) (any, error) {
	t, err := s.claimTarget(ctx, a, name)
	if err != nil {
		return nil, err
	}
	self := a.Descriptor()
	if self.Role != registry.RolePrimary {
		return nil, rejected("caller_not_primary")
	}
	if l := s.local(t.AgentID); l != nil {
		err = l.applyClaim(a.id, self.Name)
	} else if t.Owner != nil || t.Role != registry.RolePrimary {
		err = rejected("already_claimed")
	} else {
		err = s.backend.Claim(ctx, a, t)
	}
	if err != nil {
		return nil, err
	}
	out := claimOut{Name: t.Name, Owner: self.Name}
	out.Inform, out.InformError = s.inform(ctx, a, t.AgentID, fmt.Sprintf("%s claimed you: you are now its subagent.", self.Name))
	return out, nil
}

func (s *Server) release(ctx context.Context, a *Agent, name string) (any, error) {
	t, err := s.claimTarget(ctx, a, name)
	if err != nil {
		return nil, err
	}
	if l := s.local(t.AgentID); l != nil {
		err = l.applyRelease(a.id)
	} else if t.Owner == nil {
		err = rejected("not_claimed")
	} else {
		err = s.backend.Release(ctx, a, t)
	}
	if err != nil {
		return nil, err
	}
	out := claimOut{Name: t.Name}
	out.Inform, out.InformError = s.inform(ctx, a, t.AgentID, fmt.Sprintf("%s released you: you are primary again.", a.Name()))
	return out, nil
}

func (s *Server) inform(ctx context.Context, from *Agent, to, text string) (*Result, string) {
	r, err := s.backend.Send(ctx, from, SendRequest{To: to, Text: text, Kind: msg.Inform, Delivery: msg.Immediate})
	if err != nil {
		return nil, describe(err)
	}
	return &r, ""
}

// ApplyClaim records a claim on this agent made by ownerID through another
// shim process; the caller has checked that ownerID is a registered primary.
func (a *Agent) ApplyClaim(ownerID, ownerName string) error { return a.applyClaim(ownerID, ownerName) }

// ApplyRelease ends ownerID's claim on this agent; a non-owner is rejected.
func (a *Agent) ApplyRelease(ownerID string) error { return a.applyRelease(ownerID) }

// applyClaim records ownerID's claim in this agent's own journal, then
// republishes its descriptor as a subagent. The journal, not the descriptor,
// is authoritative for the check.
func (a *Agent) applyClaim(ownerID, ownerName string) error {
	if !a.Admit() {
		return rejected("agent_retiring")
	}
	defer a.Done()
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.j.Latest(journal.Record{Type: journal.TypeClaim}); ok && r.Claimed {
		return rejected("already_claimed")
	}
	if err := a.j.Append(journal.Record{Type: journal.TypeClaim, Owner: ownerID, Claimed: true}); err != nil {
		return err
	}
	d := a.reg.Descriptor()
	d.Role, d.Owner = registry.RoleSubagent, &ownerName
	return a.reg.Publish(d)
}

func (a *Agent) applyRelease(ownerID string) error {
	if !a.Admit() {
		return rejected("agent_retiring")
	}
	defer a.Done()
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.j.Latest(journal.Record{Type: journal.TypeClaim})
	if !ok || !r.Claimed {
		return rejected("not_claimed")
	}
	if r.Owner != ownerID {
		return rejected("not_owner")
	}
	if err := a.j.Append(journal.Record{Type: journal.TypeClaim, Owner: ownerID, Claimed: false}); err != nil {
		return err
	}
	d := a.reg.Descriptor()
	d.Role, d.Owner = registry.RolePrimary, nil
	return a.reg.Publish(d)
}
