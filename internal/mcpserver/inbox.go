package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type inboxIn struct {
	Inbox string `json:"inbox" jsonschema:"the role inbox name, without the inbox: prefix"`
}

type grantIn struct {
	Inbox string `json:"inbox" jsonschema:"the role inbox name, without the inbox: prefix"`
	To    string `json:"to" jsonschema:"name or agent_id of a session on this host"`
	Owner bool   `json:"owner,omitempty" jsonschema:"hand ownership to a member instead of letting the session attach"`
}

type inboxMemberOut struct {
	AgentID    string    `json:"agent_id"`
	Name       string    `json:"name"`
	AttachedAt time.Time `json:"attached_at"`
}

type inboxOut struct {
	Inbox   string           `json:"inbox"`
	Owner   string           `json:"owner,omitempty"`
	Members []inboxMemberOut `json:"members"`
	Grants  []string         `json:"grants,omitempty"`
}

type attachOut struct {
	inboxOut
	Created bool `json:"created"`
}

type grantOut struct {
	Inbox  string   `json:"inbox"`
	Owner  string   `json:"owner,omitempty"`
	Grants []string `json:"grants"`
}

type claimInboxOut struct {
	Inbox string `json:"inbox"`
	Owner string `json:"owner"`
}

var inboxReasons = []struct {
	err    error
	reason string
}{
	{registry.ErrInvalidInbox, "invalid_inbox"},
	{registry.ErrNotGranted, "not_granted"},
	{registry.ErrOwnerless, "ownerless"},
	{registry.ErrNotOwner, "not_owner"},
	{registry.ErrInboxOwned, "inbox_owned"},
	{registry.ErrNoSuchInbox, "no_such_inbox"},
	{registry.ErrNotMember, "not_member"},
}

func inboxError(err error) error {
	for _, r := range inboxReasons {
		if errors.Is(err, r.err) {
			return rejected(r.reason)
		}
	}
	return err
}

func (s *Server) addInboxTools() {
	boundTool(s, &mcp.Tool{
		Name:        "attach",
		Description: "Join a role inbox that your user or launch brief named, creating it with you as owner if it does not exist. An owned inbox needs a grant from its owner. Returns {inbox, owner, members, created}.",
		InputSchema: schemaFor[inboxIn](nil),
	}, func(ctx context.Context, a *Agent, in inboxIn) (any, error) { return s.attach(ctx, a, in.Inbox) })
	boundTool(s, &mcp.Tool{
		Name:        "detach",
		Description: "Leave a role inbox. An owner that detaches leaves the inbox ownerless and keeps its grant. Returns {inbox, owner, members}.",
		InputSchema: schemaFor[inboxIn](nil),
	}, func(ctx context.Context, a *Agent, in inboxIn) (any, error) { return s.detach(ctx, a, in.Inbox) })
	boundTool(s, &mcp.Tool{
		Name:        "grant",
		Description: "As the owner of a role inbox, let a session on this host attach to it; with owner=true, hand ownership to a member instead. Returns {inbox, owner, grants}.",
		InputSchema: schemaFor[grantIn](nil),
	}, func(ctx context.Context, a *Agent, in grantIn) (any, error) { return s.grant(ctx, a, in) })
}

func (a *Agent) inboxHook(ev registry.InboxEvent) error {
	return a.j.Append(journal.Record{Type: journal.TypeAttach, Inbox: ev.Inbox, Attached: ev.Attached, At: ev.At})
}

func (a *Agent) requirePrimary() error {
	if a.Descriptor().Role != registry.RolePrimary {
		return rejected("caller_not_primary")
	}
	if a.afterPrimaryCheck != nil {
		a.afterPrimaryCheck()
	}
	return nil
}

// lockPrimary takes a.mu and re-checks the role under it, so a concurrent
// claim cannot make the caller a subagent between the check and the mutation.
// On success the caller holds a.mu.
func (a *Agent) lockPrimary() error {
	a.mu.Lock()
	if a.Descriptor().Role != registry.RolePrimary {
		a.mu.Unlock()
		return rejected("caller_not_primary")
	}
	return nil
}

func (a *Agent) inboxResult(st registry.InboxState) inboxOut {
	out := inboxOut{Inbox: st.Name, Owner: st.Owner, Members: []inboxMemberOut{}}
	for _, m := range st.Members {
		out.Members = append(out.Members, inboxMemberOut{AgentID: m.AgentID, Name: m.Name, AttachedAt: m.AttachedAt})
	}
	if st.Owner == a.id {
		out.Grants = st.Grants
	}
	return out
}

func (a *Agent) inAttached(name string) bool {
	for _, m := range a.Descriptor().Inboxes {
		if m.Name == registry.CanonInbox(name) {
			return true
		}
	}
	return false
}

func (s *Server) notifyInbox(ctx context.Context, a *Agent, change string, st registry.InboxState) {
	if n, ok := s.backend.(InboxNotifier); ok {
		n.NotifyInbox(ctx, a, change, st)
	}
}

func (s *Server) attach(ctx context.Context, a *Agent, name string) (any, error) {
	if err := a.requirePrimary(); err != nil {
		return nil, err
	}
	if err := a.lockPrimary(); err != nil {
		return nil, err
	}
	had := a.inAttached(name)
	st, err := s.reg.AttachInbox(a.reg, name, a.inboxHook)
	a.mu.Unlock()
	if err != nil {
		return nil, inboxError(err)
	}
	if !had {
		s.notifyInbox(ctx, a, "attached to the inbox", st)
	}
	return attachOut{inboxOut: a.inboxResult(st), Created: st.Created}, nil
}

func (s *Server) detach(ctx context.Context, a *Agent, name string) (any, error) {
	if err := a.requirePrimary(); err != nil {
		return nil, err
	}
	if err := a.lockPrimary(); err != nil {
		return nil, err
	}
	had := a.inAttached(name)
	st, err := s.reg.DetachInbox(a.reg, name, a.inboxHook)
	a.mu.Unlock()
	if err != nil {
		return nil, inboxError(err)
	}
	if had {
		s.notifyInbox(ctx, a, "detached from the inbox", st)
	}
	return a.inboxResult(st), nil
}

func (s *Server) grant(ctx context.Context, a *Agent, in grantIn) (any, error) {
	if err := a.requirePrimary(); err != nil {
		return nil, err
	}
	if err := registry.ValidInboxName(in.Inbox); err != nil {
		return nil, inboxError(err)
	}
	t, err := s.claimTarget(ctx, a, in.To)
	if err != nil {
		return nil, err
	}
	if err := a.lockPrimary(); err != nil {
		return nil, err
	}
	st, err := s.reg.GrantInbox(a.reg, in.Inbox, t.AgentID, in.Owner)
	a.mu.Unlock()
	if err != nil {
		return nil, inboxError(err)
	}
	if in.Owner {
		s.notifyInbox(ctx, a, "handed ownership to "+t.Name, st)
	}
	return grantOut{Inbox: st.Name, Owner: st.Owner, Grants: st.Grants}, nil
}

func (s *Server) claimInbox(ctx context.Context, a *Agent, name string) (any, error) {
	if err := a.requirePrimary(); err != nil {
		return nil, err
	}
	if err := a.lockPrimary(); err != nil {
		return nil, err
	}
	st, err := s.reg.ClaimInbox(a.reg, name, a.inboxHook)
	a.mu.Unlock()
	if err != nil {
		return nil, inboxError(err)
	}
	s.notifyInbox(ctx, a, "claimed the inbox", st)
	return claimInboxOut{Inbox: st.Name, Owner: st.Owner}, nil
}
