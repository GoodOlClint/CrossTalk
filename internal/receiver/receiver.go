package receiver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type Ingress string

const (
	UDS Ingress = "uds"
	A2A Ingress = "a2a"
)

const (
	PrincipalLocalUser = "local-user"
	unverifiedKey      = "unverified"
	LabelUnverified    = "unverified"
)

const (
	ReasonInvalid           = "invalid"
	ReasonUnknownKind       = "unknown_kind"
	ReasonHopRepeat         = "hop_repeat"
	ReasonHopOverflow       = "hop_overflow"
	ReasonPolicy            = "policy"
	ReasonUDSKind           = "uds_kind"
	ReasonTooLarge          = "too_large"
	ReasonTooManyPending    = "too_many_pending"
	ReasonCanceled          = "canceled"
	ReasonDuplicateDispatch = "duplicate_dispatch"
	ReasonJournal           = "journal"
)

// Sender is the identity an ingress resolved from transport evidence.
// AgentID is set only for a registered agent; Principal is set for an
// authenticated principal without registration (the A2A peer token).
type Sender struct {
	Verified  bool
	Principal string
	AgentID   string
	Name      string
	Role      string
	PID       int
}

func (s Sender) registered() bool { return s.Verified && s.AgentID != "" }

func (s Sender) key() string {
	switch {
	case !s.Verified:
		return unverifiedKey
	case s.AgentID != "":
		return s.AgentID
	}
	return s.Principal
}

func (s Sender) label() string {
	switch {
	case !s.Verified:
		return LabelUnverified
	case s.AgentID != "":
		return s.Name
	}
	return s.Principal
}

type Inbound struct {
	Ingress Ingress
	Message msg.Message
	Sender  Sender
	// Native marks a frame with no CrossTalk metadata: it gets a fresh
	// message_id and is exempt from message_id dedupe (§5 F6).
	Native bool
}

type Result struct {
	MessageID string
	State     adapter.State
	Reason    string
	InReplyTo *string
	// Duplicate reports that State is the prior state of an earlier
	// acceptance of the same (sender, message_id); it may be in flight
	// ("submitted", "dispatching").
	Duplicate bool
}

// Recipient is the agent this receiver serves, read per message because a
// claim can change its role and owner.
type Recipient struct {
	AgentID string
	Name    string
	Role    string
	Owner   string
	Binding adapter.Binding
}

type Limits struct {
	MaxMessageBytes int
	MaxPending      int
}

var DefaultLimits = Limits{MaxMessageBytes: 64 << 10, MaxPending: 256}

type Config struct {
	Journal *journal.Journal
	Adapter adapter.Adapter
	Self    func() Recipient
	Limits  Limits
	Now     func() time.Time
}

type route struct {
	Session   string
	MessageID string
	Sender    string
	Name      string
	Kind      msg.Kind
}

type Receiver struct {
	j      *journal.Journal
	ad     adapter.Adapter
	self   func() Recipient
	limits Limits
	now    func() time.Time

	mu      sync.Mutex
	pending int
	routes  []route

	beforeClaim func(messageID string)
}

func New(c Config) *Receiver {
	if c.Limits.MaxMessageBytes == 0 {
		c.Limits.MaxMessageBytes = DefaultLimits.MaxMessageBytes
	}
	if c.Limits.MaxPending == 0 {
		c.Limits.MaxPending = DefaultLimits.MaxPending
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Receiver{j: c.Journal, ad: c.Adapter, self: c.Self, limits: c.Limits, now: c.Now}
}

func rejected(id, reason string) Result {
	return Result{MessageID: id, State: adapter.Rejected, Reason: reason}
}

// Receive runs the single receiver chain (§5): authenticate (identity is
// resolved by the ingress) → dedupe → hop_chain → policy and admission →
// dispatch claim → inject → record outcome.
func (r *Receiver) Receive(ctx context.Context, in Inbound) Result {
	return r.receive(ctx, in, false)
}

// Resume re-runs the chain for a message accepted before a restart. A dedupe
// record still submitted with no dispatch claim proves nothing reached the
// harness, so the chain continues from policy; a claim without an outcome is
// unknown (reason journal) and is never re-injected. Any other prior state is
// returned as a duplicate, as Receive does.
func (r *Receiver) Resume(ctx context.Context, in Inbound) Result {
	return r.receive(ctx, in, true)
}

func (r *Receiver) receive(ctx context.Context, in Inbound, resume bool) Result {
	self := r.self()
	m := in.Message
	if in.Native {
		m.MessageID = msg.NewUUIDv7()
		m.InReplyTo = nil
		m.HopChain = nil
	}
	if m.ContextID == "" {
		m.ContextID = m.MessageID
	}
	if m.Delivery == "" {
		m.Delivery = msg.Immediate
	}
	if m.SentAt.IsZero() {
		m.SentAt = r.now().UTC()
	}
	if m.To == "" {
		m.To = self.Name
	}
	m.From = in.Sender.label()

	if err := m.Validate(); err != nil {
		switch {
		case errors.Is(err, msg.ErrUnknownKind):
			return rejected(m.MessageID, ReasonUnknownKind)
		case errors.Is(err, msg.ErrHopRepeat), errors.Is(err, msg.ErrHopOverflow):
		default:
			return rejected(m.MessageID, ReasonInvalid)
		}
	}

	key := in.Sender.key()
	body, err := json.Marshal(m)
	if err != nil {
		return rejected(m.MessageID, ReasonInvalid)
	}
	prior, dup, err := r.j.AcceptRecord(journal.Record{Sender: key, MessageID: m.MessageID, State: journal.StateSubmitted, Message: body})
	if err != nil {
		return rejected(m.MessageID, ReasonJournal)
	}
	if dup {
		if !resume || prior != journal.StateSubmitted {
			return Result{MessageID: m.MessageID, State: adapter.State(prior), Duplicate: true}
		}
		if l, claimed := r.j.DispatchState(m.MessageID); claimed {
			switch l.Type {
			case journal.TypeCancel:
				return rejected(m.MessageID, ReasonCanceled)
			case journal.TypeOutcome:
				return Result{MessageID: m.MessageID, State: adapter.State(l.State), Duplicate: true}
			}
			return Result{MessageID: m.MessageID, State: adapter.Unknown, Reason: ReasonJournal, Duplicate: true}
		}
	}
	reject := func(reason string) Result {
		r.j.SetDedupeState(key, m.MessageID, string(adapter.Rejected))
		return rejected(m.MessageID, reason)
	}

	if reason := hopCheck(m.HopChain, self.AgentID); reason != "" {
		return reject(reason)
	}
	if reason := policy(in.Ingress, m.Kind, in.Sender, self); reason != "" {
		return reject(reason)
	}
	if textBytes(m) > r.limits.MaxMessageBytes {
		return reject(ReasonTooLarge)
	}
	if !r.reserve() {
		return reject(ReasonTooManyPending)
	}
	defer r.unreserve()

	if in.Native && in.Sender.registered() && in.Sender.AgentID == self.AgentID {
		return r.nativeReply(ctx, key, m, self)
	}

	if r.beforeClaim != nil {
		r.beforeClaim(m.MessageID)
	}
	ok, err := r.j.Claim(m.MessageID)
	if err != nil {
		return reject(ReasonJournal)
	}
	if !ok {
		if l, _ := r.j.DispatchState(m.MessageID); l.Type == journal.TypeCancel {
			r.j.SetDedupeState(key, m.MessageID, journal.StateCanceled)
			return rejected(m.MessageID, ReasonCanceled)
		}
		return reject(ReasonDuplicateDispatch)
	}

	rt := route{Session: self.Binding.SessionRef, MessageID: m.MessageID, Sender: in.Sender.AgentID, Name: in.Sender.Name, Kind: m.Kind}
	routed := in.Sender.registered()
	if routed {
		if err := r.openRoute(rt); err != nil {
			r.j.Resolve(m.MessageID, string(adapter.Rejected))
			return reject(ReasonJournal)
		}
	}
	st, _ := r.ad.Inject(ctx, m, adapter.Delivery{Mode: m.Delivery, Target: self.Binding, VerifiedSender: in.Sender.Verified})
	if !st.Injectable() {
		st = adapter.Unknown
	}
	resolveErr := r.j.Resolve(m.MessageID, string(st))
	dedupeErr := r.j.SetDedupeState(key, m.MessageID, string(st))
	if resolveErr != nil || dedupeErr != nil {
		r.j.SetDedupeState(key, m.MessageID, string(adapter.Unknown))
		return Result{MessageID: m.MessageID, State: adapter.Unknown, Reason: ReasonJournal}
	}
	if routed && (st == adapter.Rejected || st == adapter.Unreachable) {
		r.closeRoute(rt)
	}
	return Result{MessageID: m.MessageID, State: st}
}

func hopCheck(chain []string, selfID string) string {
	switch err := msg.CheckHopChain(chain); {
	case errors.Is(err, msg.ErrHopOverflow):
		return ReasonHopOverflow
	case errors.Is(err, msg.ErrHopRepeat):
		return ReasonHopRepeat
	case err != nil:
		return ReasonInvalid
	}
	if slices.Contains(chain, msg.Hop(selfID)) {
		return ReasonHopRepeat
	}
	return ""
}

// policy is the §7 table. kind is a routing hint, not a trust boundary: the
// table only keeps forged or unauthorised command away from the model.
func policy(ing Ingress, k msg.Kind, s Sender, to Recipient) string {
	switch k {
	case msg.Inform:
		return ""
	case msg.Ask, msg.Command:
	default:
		return ReasonUnknownKind
	}
	if ing != A2A {
		return ReasonUDSKind
	}
	if !s.Verified {
		return ReasonPolicy
	}
	if k == msg.Ask {
		return ""
	}
	if !s.registered() {
		return ReasonPolicy
	}
	if to.Role == registry.RoleSubagent {
		if to.Owner != "" && s.AgentID == to.Owner {
			return ""
		}
		return ReasonPolicy
	}
	if s.Role == registry.RolePrimary {
		return ""
	}
	return ReasonPolicy
}

func textBytes(m msg.Message) int {
	n := 0
	for _, p := range m.Parts {
		n += len(p.Text)
	}
	return n
}

func (r *Receiver) reserve() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending >= r.limits.MaxPending {
		return false
	}
	r.pending++
	return true
}

func (r *Receiver) unreserve() {
	r.mu.Lock()
	r.pending--
	r.mu.Unlock()
}

func (r *Receiver) openRoute(rt route) error {
	if err := r.j.Append(journal.Record{Type: journal.TypeRoute, Session: rt.Session, MessageID: rt.MessageID, Sender: rt.Sender, To: rt.Name, Kind: string(rt.Kind), State: journal.StateOpen}); err != nil {
		return err
	}
	r.mu.Lock()
	r.routes = append(r.routes, rt)
	r.mu.Unlock()
	return nil
}

func (r *Receiver) closeRoute(rt route) {
	r.j.Append(journal.Record{Type: journal.TypeRoute, Session: rt.Session, MessageID: rt.MessageID, Sender: rt.Sender, To: rt.Name, Kind: string(rt.Kind), State: journal.StateClosed})
	r.mu.Lock()
	r.routes = slices.DeleteFunc(r.routes, func(o route) bool { return o.Session == rt.Session && o.MessageID == rt.MessageID })
	r.mu.Unlock()
}

// CloseRoute removes the open route for a delivered message id and journals
// it closed; it reports whether one existed.
func (r *Receiver) CloseRoute(messageID string) bool {
	r.mu.Lock()
	idx := slices.IndexFunc(r.routes, func(o route) bool { return o.MessageID == messageID })
	if idx < 0 {
		r.mu.Unlock()
		return false
	}
	rt := r.routes[idx]
	r.routes = slices.Delete(r.routes, idx, idx+1)
	r.mu.Unlock()
	r.j.Append(journal.Record{Type: journal.TypeRoute, Session: rt.Session, MessageID: rt.MessageID, Sender: rt.Sender, To: rt.Name, Kind: string(rt.Kind), State: journal.StateClosed})
	return true
}

// nativeReply handles a native frame from this agent's own session. The frame
// carries no correlation id, so it is never forwarded: it is journaled as
// bounced and the session is told, through its own harness, which inbound
// messages it can answer with the reply tool (ADR 0009). The bounce opens no
// route and completes nothing.
func (r *Receiver) nativeReply(ctx context.Context, key string, m msg.Message, self Recipient) Result {
	r.j.SetDedupeState(key, m.MessageID, string(adapter.Bounced))
	res := Result{MessageID: m.MessageID, State: adapter.Bounced}
	st, _ := r.ad.Inject(ctx, r.bounce(m, self.Binding.SessionRef), adapter.Delivery{Mode: msg.Immediate, Target: self.Binding, VerifiedSender: true})
	if st != adapter.Accepted && st != adapter.Queued {
		res.Reason = string(st)
	}
	return res
}

func (r *Receiver) bounce(native msg.Message, session string) msg.Message {
	first, _, _ := strings.Cut(native.Text(), "\n")
	if rs := []rune(first); len(rs) > 200 {
		first = string(rs[:200])
	}
	var b strings.Builder
	b.WriteString("CrossTalk: your native message was not delivered. Do not reply to this message natively.\n")
	fmt.Fprintf(&b, "Undelivered text began: %s\n", first)
	r.mu.Lock()
	var open []route
	for _, rt := range r.routes {
		if rt.Session == session {
			open = append(open, rt)
		}
	}
	r.mu.Unlock()
	if len(open) == 0 {
		b.WriteString("No open inbound messages; use send_message(to=<name>) to start a conversation.")
	} else {
		b.WriteString("Open inbound messages you can answer with the reply tool:")
		for _, rt := range open {
			fmt.Fprintf(&b, "\n- message_id=%s from=%s kind=%s", rt.MessageID, rt.Name, rt.Kind)
		}
	}
	id := msg.NewUUIDv7()
	return msg.Message{MessageID: id, ContextID: id, From: "crosstalk", To: native.To, Kind: msg.Inform, Delivery: msg.Immediate, SentAt: r.now().UTC(), Parts: []msg.Part{{Type: "text", Text: b.String()}}}
}

func (r *Receiver) storeMessage(key string, m msg.Message, state string) {
	body, _ := json.Marshal(m)
	r.j.Append(journal.Record{Type: journal.TypeDedupe, Sender: key, MessageID: m.MessageID, State: state, Message: body})
}

// Cancel records a cancel for messageID; it fails once a dispatch claim is
// journaled, so a successful cancel means no harness write (§5 C5).
func (r *Receiver) Cancel(messageID string) (bool, error) {
	return r.j.Cancel(messageID)
}
