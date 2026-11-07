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
	ReasonRemoteKind        = "remote_kind"
	ReasonIDInUse           = "message_id_in_use"
)

// RemoteKeyPrefix starts every host-scoped sender key; no local key (an
// agent_id, a principal, "unverified") begins with it.
const RemoteKeyPrefix = "remote:"

// RemoteKey is the host-scoped key of an agent on a paired host.
func RemoteKey(host, agentID string) string { return RemoteKeyPrefix + host + "/" + agentID }

// IsRemoteKey reports whether k is a host-scoped sender key.
func IsRemoteKey(k string) bool { return strings.HasPrefix(k, RemoteKeyPrefix) }

// Sender is the identity an ingress resolved from transport evidence.
// AgentID is set only for a registered agent; Principal is set for an
// authenticated principal without registration (the A2A peer token).
type Sender struct {
	Verified  bool
	Principal string
	AgentID   string
	Name      string
	Role      string
	Harness   string
	PID       int
	// Host is the paired host's certificate fingerprint and HostAlias the
	// receiver's own name for it; both are set only for a remote sender.
	Host      string
	HostAlias string
}

// Key is the dedupe, receipt and route key of the sender.
func (s Sender) Key() string { return s.key() }

func (s Sender) remote() bool { return s.Host != "" }

// registered reports a sender that is a registered agent of this host. A
// remote sender never is: it cannot satisfy a path that compares AgentID with
// a local agent.
func (s Sender) registered() bool { return s.Verified && s.AgentID != "" && !s.remote() }

// routable reports a sender a reply can be addressed to: a local registered
// agent or an agent on a paired host.
func (s Sender) routable() bool { return s.Verified && s.AgentID != "" }

func (s Sender) key() string {
	switch {
	case !s.Verified:
		return unverifiedKey
	case s.remote():
		return RemoteKey(s.Host, s.AgentID)
	case s.AgentID != "":
		return s.AgentID
	}
	return s.Principal
}

// dispatchScope is the namespace of the sender's dispatch claims: empty for a
// local sender, the host-scoped key for a remote one.
func (s Sender) dispatchScope() string {
	if s.remote() {
		return s.key()
	}
	return ""
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

const (
	// InformRouteTTL is how long an inform's route stays open after its
	// journaled open time (ADR 0010); an ask's route follows its task.
	InformRouteTTL = journal.InformRouteTTL
	// BounceInformCap bounds the inform routes a bounce lists.
	BounceInformCap = 10
)

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
	OpenedAt  time.Time
}

func (rt route) expired(now time.Time) bool {
	return rt.Kind != msg.Ask && now.Sub(rt.OpenedAt) > InformRouteTTL
}

func (rt route) record(state string) journal.Record {
	return journal.Record{Type: journal.TypeRoute, Session: rt.Session, MessageID: rt.MessageID, Sender: rt.Sender, To: rt.Name, Kind: string(rt.Kind), State: state}
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
	scope := in.Sender.dispatchScope()
	body, err := json.Marshal(m)
	if err != nil {
		return rejected(m.MessageID, ReasonInvalid)
	}
	prior, dup, err := r.j.AcceptRecord(journal.Record{Sender: key, MessageID: m.MessageID, State: journal.StateSubmitted, Message: body})
	if errors.Is(err, journal.ErrIDInUse) {
		return rejected(m.MessageID, ReasonIDInUse)
	}
	if err != nil {
		return rejected(m.MessageID, ReasonJournal)
	}
	if dup {
		if !resume || prior != journal.StateSubmitted {
			return Result{MessageID: m.MessageID, State: adapter.State(prior), Duplicate: true}
		}
		if l, claimed := r.j.DispatchStateFor(scope, m.MessageID); claimed {
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

	if reason := r.admission(in.Ingress, m, in.Sender, self); reason != "" {
		return reject(reason)
	}
	if !r.reserve() {
		return reject(ReasonTooManyPending)
	}
	defer r.unreserve()

	if !in.Native && in.Sender.routable() && m.InReplyTo != nil && r.askedPeer(*m.InReplyTo, key, m.ContextID) {
		if err := r.j.Append(journal.Record{Type: journal.TypeReceipt, MessageID: m.MessageID, InReplyTo: *m.InReplyTo, Sender: key, ContextID: m.ContextID}); err != nil {
			return reject(ReasonJournal)
		}
	}

	if in.Native && in.Sender.registered() && in.Sender.AgentID == self.AgentID {
		return r.nativeReply(ctx, key, m, self)
	}

	if r.beforeClaim != nil {
		r.beforeClaim(m.MessageID)
	}
	ok, err := r.j.ClaimFor(scope, m.MessageID)
	if err != nil {
		return reject(ReasonJournal)
	}
	if !ok {
		if l, _ := r.j.DispatchStateFor(scope, m.MessageID); l.Type == journal.TypeCancel {
			r.j.SetDedupeState(key, m.MessageID, journal.StateCanceled)
			return rejected(m.MessageID, ReasonCanceled)
		}
		return reject(ReasonDuplicateDispatch)
	}

	rt := route{Session: self.Binding.SessionRef, MessageID: m.MessageID, Sender: key, Name: in.Sender.Name, Kind: m.Kind}
	routed := in.Sender.routable()
	if routed {
		if err := r.openRoute(rt); err != nil {
			r.j.ResolveFor(scope, m.MessageID, string(adapter.Rejected))
			return reject(ReasonJournal)
		}
	}
	st, _ := r.ad.Inject(ctx, m, adapter.Delivery{Mode: m.Delivery, Target: self.Binding, VerifiedSender: in.Sender.Verified, SenderHarness: in.Sender.Harness})
	if !st.Injectable() {
		st = adapter.Unknown
	}
	resolveErr := r.j.ResolveFor(scope, m.MessageID, string(st))
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

// admission runs the hop, policy and size checks and returns the first
// failing reason, or "".
func (r *Receiver) admission(ing Ingress, m msg.Message, s Sender, self Recipient) string {
	if reason := hopCheck(m.HopChain, self.AgentID); reason != "" {
		return reason
	}
	if reason := policy(ing, m.Kind, s, self); reason != "" {
		return reason
	}
	if textBytes(m) > r.limits.MaxMessageBytes {
		return ReasonTooLarge
	}
	return ""
}

// askedPeer reports whether askID is an outbound ask of this agent sent to
// agentID in conversation contextID.
func (r *Receiver) askedPeer(askID, agentID, contextID string) bool {
	l, ok := r.j.Latest(journal.Record{Type: journal.TypeOutbound, MessageID: askID})
	if !ok || l.Kind != string(msg.Ask) || l.To != agentID {
		return false
	}
	if l.ContextID != "" {
		return l.ContextID == contextID
	}
	rs, err := r.j.Records()
	if err != nil {
		return false
	}
	for _, o := range rs {
		var m msg.Message
		if o.Type == journal.TypeOutbound && o.MessageID == askID && len(o.Message) > 0 && json.Unmarshal(o.Message, &m) == nil {
			return m.ContextID == contextID
		}
	}
	return false
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
	if s.remote() {
		return ReasonRemoteKind
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
	r.sweep()
	rec := rt.record(journal.StateOpen)
	rec.At = r.now().UTC()
	if err := r.j.Append(rec); err != nil {
		return err
	}
	rt.OpenedAt = rec.At
	r.mu.Lock()
	r.routes = append(r.routes, rt)
	r.mu.Unlock()
	return nil
}

func (r *Receiver) closeRoute(rt route) {
	r.j.Append(rt.record(journal.StateClosed))
	r.mu.Lock()
	r.routes = slices.DeleteFunc(r.routes, func(o route) bool { return o.Session == rt.Session && o.MessageID == rt.MessageID })
	r.mu.Unlock()
}

// sweep removes expired inform routes, journals them closed, and returns
// the routes still open.
func (r *Receiver) sweep() []route {
	now := r.now()
	r.mu.Lock()
	var gone []route
	r.routes = slices.DeleteFunc(r.routes, func(o route) bool {
		if o.expired(now) {
			gone = append(gone, o)
			return true
		}
		return false
	})
	open := slices.Clone(r.routes)
	r.mu.Unlock()
	for _, rt := range gone {
		r.j.Append(rt.record(journal.StateClosed))
	}
	return open
}

// RouteOpen reports whether messageID has an open route; an expired inform
// route is closed first.
func (r *Receiver) RouteOpen(messageID string) bool {
	return slices.ContainsFunc(r.sweep(), func(o route) bool { return o.MessageID == messageID })
}

// CloseRoute removes the open route for a delivered message id and journals
// it closed; it reports whether one existed. An expired inform route was
// already closed and reports false.
func (r *Receiver) CloseRoute(messageID string) bool {
	r.sweep()
	r.mu.Lock()
	idx := slices.IndexFunc(r.routes, func(o route) bool { return o.MessageID == messageID })
	if idx < 0 {
		r.mu.Unlock()
		return false
	}
	rt := r.routes[idx]
	r.routes = slices.Delete(r.routes, idx, idx+1)
	r.mu.Unlock()
	r.j.Append(rt.record(journal.StateClosed))
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
	var asks, informs []route
	for _, rt := range r.sweep() {
		switch {
		case rt.Session != session:
		case rt.Kind == msg.Ask:
			asks = append(asks, rt)
		default:
			informs = append(informs, rt)
		}
	}
	slices.Reverse(informs)
	slices.SortStableFunc(informs, func(a, b route) int { return b.OpenedAt.Compare(a.OpenedAt) })
	omitted := max(0, len(informs)-BounceInformCap)
	informs = informs[:len(informs)-omitted]
	if len(asks)+len(informs) == 0 {
		b.WriteString("No open inbound messages; use send_message(to=<name>) to start a conversation.")
	} else {
		b.WriteString("Open inbound messages you can answer with the reply tool:")
		for _, rt := range append(asks, informs...) {
			fmt.Fprintf(&b, "\n- message_id=%s from=%s kind=%s", rt.MessageID, rt.Name, rt.Kind)
		}
		if omitted > 0 {
			fmt.Fprintf(&b, "\nand %d more informs", omitted)
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

// IDHeldByOther reports whether messageID is already held by a sender other
// than the one whose key is senderKey; the A2A ingress asks before it
// publishes a task, since reply and task lookups select by message_id.
func (r *Receiver) IDHeldByOther(senderKey, messageID string) bool {
	return r.j.IDHeldByOther(senderKey, messageID)
}

// CancelFrom is Cancel in the namespace of the sender whose key is senderKey.
func (r *Receiver) CancelFrom(senderKey, messageID string) (bool, error) {
	return r.j.CancelFor(scopeOfKey(senderKey), messageID)
}

// scopeOfKey maps a dedupe sender key to its dispatch scope.
func scopeOfKey(k string) string {
	if IsRemoteKey(k) {
		return k
	}
	return ""
}
