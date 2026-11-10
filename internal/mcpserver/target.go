package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// Target is a resolved message target: a registered agent, or the native
// sessions the target matched (several only for an ambiguous name).
type Target struct {
	Desc       registry.Descriptor
	Registered bool
	Native     []Session
	// Remote is set when the value ends in @<paired alias>, whether or not a
	// local session also matched it; Ref is what precedes the last @.
	Remote *RemoteRef
	// UnknownAlias is the text after the last @ when nothing local matched and
	// it is no paired alias.
	UnknownAlias string
	// Inbox is set when to had the inbox: prefix. It never carries a session.
	Inbox *InboxTarget
	// Rejection is the refusal for an inbox address that cannot be sent to.
	Rejection *Error
}

// InboxTarget is a role inbox and its live members, responder first.
type InboxTarget struct {
	Name    string
	Members []registry.Descriptor
}

// RemoteRef is a name or agent_id on a paired host.
type RemoteRef struct{ Alias, Ref string }

// RemoteAddresser is optionally implemented by a Backend that knows the paired
// hosts' aliases.
type RemoteAddresser interface {
	IsPairedAlias(alias string) bool
}

// RemoteChecker is optionally implemented by a Backend that can ask a paired
// host whether it lists a session the value also reaches.
type RemoteChecker interface {
	CheckRemote(ctx context.Context, rr RemoteRef) (agentID string, collides bool, err error)
}

func aliasChecker(b Backend) func(string) bool {
	if ra, ok := b.(RemoteAddresser); ok {
		return ra.IsPairedAlias
	}
	return nil
}

// SplitAddress splits to at its last "@" into ref and alias; both must be non-empty.
func SplitAddress(to string) (ref, alias string, ok bool) {
	i := strings.LastIndex(to, "@")
	if i <= 0 || i == len(to)-1 {
		return "", "", false
	}
	return to[:i], to[i+1:], true
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
// registration. An empty to matches nothing. A to that ends in @ and a name
// isAlias accepts also carries Remote; one that matches nothing locally and
// ends in any other @ text carries UnknownAlias. isAlias may be nil.
func ResolveTarget(ctx context.Context, reg *registry.Registry, discover func(context.Context) ([]Session, error), isAlias func(string) bool, to string) (Target, error) {
	if name, ok := identity.CutInbox(to); ok {
		return resolveInbox(reg, name)
	}
	t, err := resolveLocal(ctx, reg, discover, to)
	if err != nil {
		return t, err
	}
	if ref, alias, ok := SplitAddress(to); ok {
		switch {
		case isAlias != nil && isAlias(alias):
			t.Remote = &RemoteRef{Alias: alias, Ref: ref}
		case !t.Registered && len(t.Native) == 0:
			t.UnknownAlias = alias
		}
	}
	return t, nil
}

// resolveInbox reads the inbox: prefix before any address split, so a value
// with it never reaches name lookup, discovery or the paired-alias path.
func resolveInbox(reg *registry.Registry, name string) (Target, error) {
	if strings.Contains(name, "@") {
		return Target{Rejection: rejected("remote_inbox_unsupported")}, nil
	}
	if err := registry.ValidInboxName(name); err != nil {
		return Target{Rejection: rejected("invalid_inbox")}, nil
	}
	name = registry.CanonInbox(name)
	ds, err := reg.InboxMembers(name)
	if err != nil {
		return Target{}, fmt.Errorf("registry: %w", err)
	}
	t := Target{Inbox: &InboxTarget{Name: name, Members: ds}}
	if len(ds) == 0 {
		t.Rejection = &Error{Code: CodeRejected, Reason: "no_such_agent", Hint: fmt.Sprintf("No session is attached to the inbox %s. Run list_agents to see which inboxes have members, or ask your user; a session joins an inbox with attach.", name)}
	}
	return t, nil
}

func resolveLocal(ctx context.Context, reg *registry.Registry, discover func(context.Context) ([]Session, error), to string) (Target, error) {
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
