package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/GoodOlClint/crosstalk/internal/identity"
)

var (
	ErrInvalidInbox = errors.New("registry: invalid inbox name")
	ErrNotGranted   = errors.New("registry: inbox is owned and the caller is not granted")
	ErrOwnerless    = errors.New("registry: inbox is ownerless and the caller is not granted")
	ErrNotOwner     = errors.New("registry: caller does not own the inbox")
	ErrInboxOwned   = errors.New("registry: inbox has a live owner")
	ErrNoSuchInbox  = errors.New("registry: no such inbox")
	ErrNotMember    = errors.New("registry: target is not a member of the inbox")
)

// InboxFile is the persisted ownership state of one inbox.
type InboxFile struct {
	Name   string   `json:"name"`
	Owner  string   `json:"owner"`
	Grants []string `json:"grants"`
}

// InboxEvent reports a change to the calling agent's own membership. The
// hook that receives it runs after the inbox file is written and before the
// descriptor is published; an error from it aborts the operation there.
type InboxEvent struct {
	Inbox    string
	Attached bool
	At       time.Time
}

type InboxHook func(InboxEvent) error

type InboxMember struct {
	AgentID    string
	Name       string
	AttachedAt time.Time
}

// InboxState is the inbox after an operation. Owner is empty when the inbox
// is ownerless. Members are responder first.
type InboxState struct {
	Name    string
	Owner   string
	Grants  []string
	Members []InboxMember
	Created bool
}

func (r *Registry) inboxesDir() string { return filepath.Join(r.Dir, "inboxes") }

func (r *Registry) inboxPath(name string) string {
	return filepath.Join(r.inboxesDir(), fileSafe(name)+".json")
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func ValidInboxName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty", ErrInvalidInbox)
	case strings.Contains(name, "@"):
		return fmt.Errorf("%w: contains @", ErrInvalidInbox)
	case strings.Contains(name, "/"):
		return fmt.Errorf("%w: contains /", ErrInvalidInbox)
	}
	if _, ok := identity.CutInbox(name); ok {
		return fmt.Errorf("%w: starts with %s", ErrInvalidInbox, identity.InboxPrefix)
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return fmt.Errorf("%w: control character", ErrInvalidInbox)
		}
	}
	return nil
}

// lockInbox blocks until it holds the exclusive inbox lock; closing the
// returned file releases it.
func (r *Registry) lockInbox(name string) (*os.File, error) {
	if err := os.MkdirAll(r.inboxesDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(r.inboxesDir(), fileSafe(name)+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ReadInbox returns the inbox file; ok is false when there is none.
func (r *Registry) ReadInbox(name string) (InboxFile, bool, error) {
	b, err := os.ReadFile(r.inboxPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return InboxFile{}, false, nil
	}
	if err != nil {
		return InboxFile{}, false, err
	}
	var f InboxFile
	if err := json.Unmarshal(b, &f); err != nil {
		return InboxFile{}, false, fmt.Errorf("registry: %s: %w", r.inboxPath(name), err)
	}
	return f, true, nil
}

func (r *Registry) writeInbox(f InboxFile) error {
	if f.Grants == nil {
		f.Grants = []string{}
	}
	return writeJSONAtomic(r.inboxPath(f.Name), f)
}

// holdsInbox reports whether agentID has a live, non-superseded descriptor
// that lists the inbox.
func (r *Registry) holdsInbox(agentID, name string) bool {
	if agentID == "" {
		return false
	}
	d, err := readDescriptor(r.DescriptorPath(agentID))
	if err != nil || !r.Live(d) || r.IsSuperseded(d) {
		return false
	}
	return slices.ContainsFunc(d.Inboxes, func(m InboxMembership) bool { return m.Name == name })
}

// InboxMembers returns the live, non-superseded descriptors that list the
// inbox, responder first: latest attached_at, ties to the lower agent_id.
func (r *Registry) InboxMembers(name string) ([]Descriptor, error) {
	ds, err := r.List()
	if err != nil {
		return nil, err
	}
	at := func(d Descriptor) (time.Time, bool) {
		for _, m := range d.Inboxes {
			if m.Name == name {
				return m.AttachedAt, true
			}
		}
		return time.Time{}, false
	}
	var out []Descriptor
	for _, d := range ds {
		if _, ok := at(d); ok {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Descriptor) int {
		ta, _ := at(a)
		tb, _ := at(b)
		if c := tb.Compare(ta); c != 0 {
			return c
		}
		return strings.Compare(a.AgentID, b.AgentID)
	})
	return out, nil
}

func (r *Registry) inboxState(name string, f InboxFile, created bool) (InboxState, error) {
	ds, err := r.InboxMembers(name)
	if err != nil {
		return InboxState{}, err
	}
	st := InboxState{Name: name, Grants: slices.Clone(f.Grants), Created: created}
	if r.holdsInbox(f.Owner, name) {
		st.Owner = f.Owner
	}
	for _, d := range ds {
		m := InboxMember{AgentID: d.AgentID, Name: d.Name}
		for _, im := range d.Inboxes {
			if im.Name == name {
				m.AttachedAt = im.AttachedAt
			}
		}
		st.Members = append(st.Members, m)
	}
	return st, nil
}

func addUnique(list []string, id string) []string {
	if slices.Contains(list, id) {
		return list
	}
	return append(list, id)
}

func (r *Registry) join(g *Registration, name string, at time.Time, hook InboxHook) error {
	if err := hook(InboxEvent{Inbox: name, Attached: true, At: at}); err != nil {
		return err
	}
	list := slices.DeleteFunc(slices.Clone(g.Descriptor().Inboxes), func(m InboxMembership) bool { return m.Name == name })
	return g.setInboxes(append(list, InboxMembership{Name: name, AttachedAt: at}))
}

func (r *Registry) leave(g *Registration, name string, hook InboxHook) error {
	if err := hook(InboxEvent{Inbox: name, Attached: false, At: r.now()}); err != nil {
		return err
	}
	return g.setInboxes(slices.DeleteFunc(slices.Clone(g.Descriptor().Inboxes), func(m InboxMembership) bool { return m.Name == name }))
}

// AttachInbox adds g to the inbox, creating the inbox with g as owner when it
// has no file. A caller already in the inbox succeeds without change.
func (r *Registry) AttachInbox(g *Registration, name string, hook InboxHook) (InboxState, error) {
	if err := ValidInboxName(name); err != nil {
		return InboxState{}, err
	}
	l, err := r.lockInbox(name)
	if err != nil {
		return InboxState{}, err
	}
	defer l.Close()
	f, ok, err := r.ReadInbox(name)
	if err != nil {
		return InboxState{}, err
	}
	if g.hasInbox(name) {
		return r.inboxState(name, f, false)
	}
	me := g.Descriptor().AgentID
	created := false
	switch {
	case !ok:
		f = InboxFile{Name: name, Owner: me, Grants: []string{}}
		if err := r.writeInbox(f); err != nil {
			return InboxState{}, err
		}
		created = true
	case slices.Contains(f.Grants, me):
	case r.holdsInbox(f.Owner, name):
		return InboxState{}, ErrNotGranted
	default:
		return InboxState{}, ErrOwnerless
	}
	if err := r.join(g, name, r.now(), hook); err != nil {
		return InboxState{}, err
	}
	return r.inboxState(name, f, created)
}

// DetachInbox removes g from the inbox. An owner that detaches leaves the
// inbox ownerless and is added to its grants.
func (r *Registry) DetachInbox(g *Registration, name string, hook InboxHook) (InboxState, error) {
	if err := ValidInboxName(name); err != nil {
		return InboxState{}, err
	}
	l, err := r.lockInbox(name)
	if err != nil {
		return InboxState{}, err
	}
	defer l.Close()
	f, ok, err := r.ReadInbox(name)
	if err != nil {
		return InboxState{}, err
	}
	if !g.hasInbox(name) {
		return r.inboxState(name, f, false)
	}
	if me := g.Descriptor().AgentID; ok && f.Owner == me {
		f.Owner, f.Grants = "", addUnique(f.Grants, me)
		if err := r.writeInbox(f); err != nil {
			return InboxState{}, err
		}
	}
	if err := r.leave(g, name, hook); err != nil {
		return InboxState{}, err
	}
	return r.inboxState(name, f, false)
}

// GrantInbox lets the agent to attach. With handOver it makes to the owner
// instead, which must already be a member, and adds the old owner to the
// grants; only the live owner may grant.
func (r *Registry) GrantInbox(g *Registration, name, to string, handOver bool) (InboxState, error) {
	if err := ValidInboxName(name); err != nil {
		return InboxState{}, err
	}
	l, err := r.lockInbox(name)
	if err != nil {
		return InboxState{}, err
	}
	defer l.Close()
	f, ok, err := r.ReadInbox(name)
	if err != nil {
		return InboxState{}, err
	}
	if !ok {
		return InboxState{}, ErrNoSuchInbox
	}
	me := g.Descriptor().AgentID
	if f.Owner != me || !r.holdsInbox(me, name) {
		return InboxState{}, ErrNotOwner
	}
	if handOver {
		if !r.holdsInbox(to, name) {
			return InboxState{}, ErrNotMember
		}
		if to != me {
			f.Owner, f.Grants = to, addUnique(f.Grants, me)
		}
	} else {
		f.Grants = addUnique(f.Grants, to)
	}
	if err := r.writeInbox(f); err != nil {
		return InboxState{}, err
	}
	return r.inboxState(name, f, false)
}

// ClaimInbox makes g the owner and a member of an ownerless inbox, which keeps
// its grants. The live owner claiming again succeeds without change.
func (r *Registry) ClaimInbox(g *Registration, name string, hook InboxHook) (InboxState, error) {
	if err := ValidInboxName(name); err != nil {
		return InboxState{}, err
	}
	l, err := r.lockInbox(name)
	if err != nil {
		return InboxState{}, err
	}
	defer l.Close()
	f, ok, err := r.ReadInbox(name)
	if err != nil {
		return InboxState{}, err
	}
	if !ok {
		return InboxState{}, ErrNoSuchInbox
	}
	me := g.Descriptor().AgentID
	if r.holdsInbox(f.Owner, name) {
		if f.Owner == me {
			return r.inboxState(name, f, false)
		}
		return InboxState{}, ErrInboxOwned
	}
	f.Owner = me
	if err := r.writeInbox(f); err != nil {
		return InboxState{}, err
	}
	if !g.hasInbox(name) {
		if err := r.join(g, name, r.now(), hook); err != nil {
			return InboxState{}, err
		}
	}
	return r.inboxState(name, f, false)
}

// ResumeInbox restores a journaled membership under the inbox lock. The
// session keeps it, with its original attached_at, when the file names it
// owner or lists it in grants; otherwise the membership is dropped and the
// hook reports the detach.
func (r *Registry) ResumeInbox(g *Registration, m InboxMembership, hook InboxHook) (bool, error) {
	if err := ValidInboxName(m.Name); err != nil {
		return false, err
	}
	l, err := r.lockInbox(m.Name)
	if err != nil {
		return false, err
	}
	defer l.Close()
	f, ok, err := r.ReadInbox(m.Name)
	if err != nil {
		return false, err
	}
	me := g.Descriptor().AgentID
	if !ok || f.Owner != me && !slices.Contains(f.Grants, me) {
		return false, r.leave(g, m.Name, hook)
	}
	list := slices.DeleteFunc(slices.Clone(g.Descriptor().Inboxes), func(x InboxMembership) bool { return x.Name == m.Name })
	return true, g.setInboxes(append(list, m))
}
