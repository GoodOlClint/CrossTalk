package shim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// PrincipalCrossTalk is the sender of messages the shim itself injects into
// its own session, such as an ask's deadline notice (§8).
const PrincipalCrossTalk = "crosstalk"

func itoa(i int) string { return strconv.Itoa(i) }

func (st *AgentState) newMessage(to string, k msg.Kind, d msg.Delivery, text string) msg.Message {
	id := msg.NewUUIDv7()
	return msg.Message{
		MessageID: id, ContextID: id, From: st.agent.Name(), To: to, Kind: k, Delivery: d,
		HopChain: []string{msg.Hop(st.agent.ID())}, SentAt: st.shim.now(), Parts: []msg.Part{{Type: "text", Text: text}},
	}
}

func (st *AgentState) outbound(m msg.Message, to string, state adapter.State, reason string, withBody bool) {
	st.outboundTask(m, to, state, reason, withBody, "")
}

// outboundTask also keeps the far side's task id, when the Transport reports one.
func (st *AgentState) outboundTask(m msg.Message, to string, state adapter.State, reason string, withBody bool, taskID string) {
	r := journal.Record{Type: journal.TypeOutbound, MessageID: m.MessageID, To: to, Kind: string(m.Kind), State: string(state), Reason: reason, TaskID: taskID}
	if m.InReplyTo != nil {
		r.InReplyTo = *m.InReplyTo
	}
	if withBody {
		r.Message, _ = json.Marshal(m)
	}
	if err := st.agent.Journal().Append(r); err != nil {
		st.shim.log.Warn("shim: journal outbound", "agent", st.agent.ID(), "message_id", m.MessageID, "err", err)
	}
}

func (st *AgentState) viaTransport(ctx context.Context, to registry.Descriptor, m msg.Message) (adapter.State, string, string) {
	t := st.shim.opt.Transport
	if t == nil {
		return adapter.Unreachable, ReasonNoTransport, ""
	}
	var (
		taskID string
		state  adapter.State
		err    error
	)
	if tt, ok := t.(TaskTransport); ok {
		taskID, state, err = tt.SendTask(ctx, st.agent.Descriptor(), to, m, true)
	} else {
		state, err = t.Send(ctx, st.agent.Descriptor(), to, m, true)
	}
	if !state.Injectable() {
		state = adapter.Unknown
	}
	if err != nil {
		return state, err.Error(), taskID
	}
	return state, "", taskID
}

// deliver sends m by name: a descriptor means a shim-served agent (Transport);
// otherwise a native session injected through this agent's adapters.
// Every outbound is journaled before and after the attempt.
func (st *AgentState) deliver(ctx context.Context, m msg.Message) mcpserver.Result {
	res := mcpserver.Result{MessageID: m.MessageID}
	finish := func(to string, state adapter.State, reason string) mcpserver.Result {
		st.outbound(m, to, state, reason, false)
		res.State, res.Reason = state, reason
		return res
	}
	d, ok, err := st.shim.reg.Lookup(m.To)
	if err != nil {
		return finish(m.To, adapter.Unreachable, "registry: "+err.Error())
	}
	if ok {
		if d.AgentID == st.agent.ID() {
			return finish(d.AgentID, adapter.Rejected, ReasonSelf)
		}
		st.outbound(m, d.AgentID, journal.StateSubmitted, "", true)
		state, reason, taskID := st.viaTransport(ctx, d, m)
		st.outboundTask(m, d.AgentID, state, reason, false, taskID)
		res.State, res.Reason = state, reason
		return res
	}
	if m.Kind != msg.Inform {
		return finish(m.To, adapter.Rejected, ReasonNativeInformOnly)
	}
	b, ad, state, reason := st.nativeTarget(ctx, m.To)
	if ad == nil {
		return finish(m.To, state, reason)
	}
	to := string(b.Harness) + ":" + b.SessionRef
	st.outbound(m, to, journal.StateSubmitted, "", true)
	state, err = ad.Inject(ctx, m, adapter.Delivery{Mode: m.Delivery, Target: b, VerifiedSender: true})
	if !state.Injectable() {
		state = adapter.Unknown
	}
	reason = ""
	if err != nil {
		reason = err.Error()
	}
	return finish(to, state, reason)
}

func (s *Shim) Send(ctx context.Context, from *mcpserver.Agent, req mcpserver.SendRequest) (mcpserver.Result, error) {
	st, err := s.state(from)
	if err != nil {
		return mcpserver.Result{}, err
	}
	if req.To == "" {
		return mcpserver.Result{}, &mcpserver.Error{Code: mcpserver.CodeInvalid, Reason: "to"}
	}
	return st.deliver(ctx, st.newMessage(req.To, req.Kind, req.Delivery, req.Text)), nil
}

func (s *Shim) Ask(ctx context.Context, from *mcpserver.Agent, req mcpserver.AskRequest) (mcpserver.Result, error) {
	st, err := s.state(from)
	if err != nil {
		return mcpserver.Result{}, err
	}
	if req.To == "" {
		return mcpserver.Result{}, &mcpserver.Error{Code: mcpserver.CodeInvalid, Reason: "to"}
	}
	m := st.newMessage(req.To, msg.Ask, msg.Immediate, req.Text)
	m.DeadlineSeconds = req.DeadlineSeconds
	res := st.deliver(ctx, m)
	if req.DeadlineSeconds > 0 && res.State != adapter.Rejected && res.State != adapter.Unreachable {
		st.armDeadline(m, time.Duration(m.DeadlineSeconds)*time.Second)
	}
	return res, nil
}

func (st *AgentState) armDeadline(ask msg.Message, after time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return
	}
	st.deadline[ask.MessageID] = time.AfterFunc(after, func() { st.expire(ask) })
}

// rearmDeadlines restores the timers of this conversation's unanswered asks
// after a restart; the deadline is the journaled sent_at plus
// deadline_seconds, and an ask already past it expires at once.
func (st *AgentState) rearmDeadlines() {
	rs, err := st.agent.Journal().Records()
	if err != nil {
		return
	}
	cur := journal.Binding{HarnessEndpoint: st.binding.HarnessEndpoint, SessionRef: st.binding.SessionRef}
	asks := map[string]msg.Message{}
	latest := map[string]journal.Record{}
	for _, rec := range rs {
		if rec.Type != journal.TypeOutbound {
			continue
		}
		latest[rec.MessageID] = rec
		var m msg.Message
		if _, ok := asks[rec.MessageID]; ok || len(rec.Message) == 0 || json.Unmarshal(rec.Message, &m) != nil {
			continue
		}
		if m.Kind == msg.Ask && m.DeadlineSeconds > 0 && rec.Binding.SameConversation(cur) {
			asks[rec.MessageID] = m
		}
	}
	for id, ask := range asks {
		if s := adapter.State(latest[id].State); s == adapter.Rejected || s == adapter.Unreachable || st.replied(id) {
			continue
		}
		remaining := ask.SentAt.Add(time.Duration(ask.DeadlineSeconds) * time.Second).Sub(st.shim.now())
		if remaining <= 0 {
			go st.expire(ask)
			continue
		}
		st.armDeadline(ask, remaining)
	}
}

// expire tells this agent's own session that an ask got no reply in time
// (§8 V15). Work already injected at the target is not stopped.
func (st *AgentState) expire(ask msg.Message) {
	st.mu.Lock()
	delete(st.deadline, ask.MessageID)
	closed := st.closed
	st.mu.Unlock()
	if closed || st.replied(ask.MessageID) {
		return
	}
	id := msg.NewUUIDv7()
	irt := ask.MessageID
	text := fmt.Sprintf("Your ask %s to %s expired after %d s with no reply.", ask.MessageID, ask.To, ask.DeadlineSeconds)
	st.Receive(st.ctx, receiver.Inbound{
		Ingress: receiver.A2A,
		Sender:  receiver.Sender{Verified: true, Principal: PrincipalCrossTalk},
		Message: msg.Message{MessageID: id, InReplyTo: &irt, ContextID: ask.ContextID, Kind: msg.Inform, Delivery: msg.Immediate, Parts: []msg.Part{{Type: "text", Text: text}}},
	})
}

// replied reports whether any journaled inbound answers messageID.
func (st *AgentState) replied(messageID string) bool {
	rs, err := st.agent.Journal().Records()
	if err != nil {
		return false
	}
	for _, r := range rs {
		if r.Type != journal.TypeDedupe || len(r.Message) == 0 {
			continue
		}
		var m msg.Message
		if json.Unmarshal(r.Message, &m) == nil && m.InReplyTo != nil && *m.InReplyTo == messageID {
			return true
		}
	}
	return false
}

// inbound returns the journaled message messageID and the dedupe sender key
// it was accepted under (agent_id, principal, or "unverified").
func (st *AgentState) inbound(messageID string) (msg.Message, string, bool) {
	rs, err := st.agent.Journal().Records()
	if err != nil {
		return msg.Message{}, "", false
	}
	for _, r := range rs {
		if r.Type != journal.TypeDedupe || r.MessageID != messageID || len(r.Message) == 0 {
			continue
		}
		var m msg.Message
		if json.Unmarshal(r.Message, &m) == nil {
			return m, r.Sender, true
		}
	}
	return msg.Message{}, "", false
}

func (s *Shim) descriptorByID(agentID string) (registry.Descriptor, bool) {
	ds, err := s.reg.List()
	if err != nil {
		return registry.Descriptor{}, false
	}
	for _, d := range ds {
		if d.AgentID == agentID {
			return d, true
		}
	}
	return registry.Descriptor{}, false
}

func isAgentID(k string) bool {
	if len(k) != 16 {
		return false
	}
	for _, c := range k {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Reply answers an inbound message; the destination is the stored original's
// verified sender, never the model's text (§8, §5 F4).
func (s *Shim) Reply(ctx context.Context, from *mcpserver.Agent, messageID, text string) (mcpserver.Result, error) {
	st, err := s.state(from)
	if err != nil {
		return mcpserver.Result{}, err
	}
	orig, key, ok := st.inbound(messageID)
	if !ok {
		return mcpserver.Result{}, &mcpserver.Error{Code: mcpserver.CodeInvalid, Reason: ReasonNoSuchMessage}
	}
	if key == receiver.LabelUnverified {
		return mcpserver.Result{}, &mcpserver.Error{Code: string(adapter.Unreachable), Reason: ReasonSenderUnverified}
	}
	r := st.newMessage(orig.From, msg.Inform, msg.Immediate, text)
	irt := orig.MessageID
	r.InReplyTo, r.ContextID = &irt, orig.ContextID
	if err := st.agent.Journal().Append(st.obligation(r, key, journal.StatePending, true)); err != nil {
		return mcpserver.Result{}, err
	}
	st.rcv.CloseRoute(orig.MessageID)
	var hookErr error
	if s.opt.OnReply != nil {
		hookErr = s.opt.OnReply(ctx, st, orig, r)
	}
	return st.sendReply(ctx, r, key, hookErr), nil
}

func (st *AgentState) obligation(r msg.Message, to, state string, withBody bool) journal.Record {
	rec := journal.Record{Type: journal.TypeObligation, MessageID: r.MessageID, InReplyTo: *r.InReplyTo, To: to, State: state}
	if withBody {
		rec.Message, _ = json.Marshal(r)
	}
	return rec
}

func (st *AgentState) sendReply(ctx context.Context, r msg.Message, key string, hookErr error) mcpserver.Result {
	res := mcpserver.Result{MessageID: r.MessageID}
	delivered := func() {
		st.agent.Journal().Append(st.obligation(r, key, journal.StateDelivered, false))
	}
	if !isAgentID(key) {
		switch {
		case st.shim.opt.OnReply == nil:
			res.State, res.Reason = adapter.Unreachable, ReasonNoReplyHook
		case hookErr != nil:
			res.State, res.Reason = adapter.Unknown, hookErr.Error()
		default:
			res.State = adapter.Accepted
			delivered()
		}
		return res
	}
	d, ok := st.shim.descriptorByID(key)
	if !ok {
		res.State, res.Reason = adapter.Unreachable, ReasonNoSuchAgent
		st.outbound(r, key, res.State, res.Reason, false)
		return res
	}
	r.To = d.Name
	st.outbound(r, d.AgentID, journal.StateSubmitted, "", true)
	var taskID string
	res.State, res.Reason, taskID = st.viaTransport(ctx, d, r)
	st.outboundTask(r, d.AgentID, res.State, res.Reason, false, taskID)
	if res.State == adapter.Accepted || res.State == adapter.Queued {
		delivered()
	}
	return res
}

// retryObligations resends replies journaled as pending under this
// conversation; the receiver dedupes on message_id, so a resend of a reply
// that did arrive is not injected twice (C4).
func (st *AgentState) retryObligations() {
	if st.shim.opt.Transport == nil {
		return
	}
	j := st.agent.Journal()
	rs, err := j.Records()
	if err != nil {
		return
	}
	cur := journal.Binding{HarnessEndpoint: st.binding.HarnessEndpoint, SessionRef: st.binding.SessionRef}
	for _, rec := range rs {
		if rec.Type != journal.TypeObligation || len(rec.Message) == 0 || !rec.Binding.SameConversation(cur) || !isAgentID(rec.To) {
			continue
		}
		if l, ok := j.Latest(rec); !ok || l.State != journal.StatePending {
			continue
		}
		var r msg.Message
		if json.Unmarshal(rec.Message, &r) != nil || r.InReplyTo == nil {
			continue
		}
		if st.ctx.Err() != nil {
			return
		}
		st.sendReply(st.ctx, r, rec.To, nil)
	}
}

type forwarder struct{ st *AgentState }

// Forward routes a native reply from this agent's own session to the
// original sender recorded in the route (§5 F4); receiver.nativeReply has
// already journaled it.
func (f forwarder) Forward(ctx context.Context, m msg.Message, toAgentID string) (adapter.State, error) {
	st := f.st
	m.From, m.Kind = st.agent.Name(), msg.Inform
	m.HopChain = []string{msg.Hop(st.agent.ID())}
	if m.InReplyTo != nil && st.shim.opt.OnReply != nil {
		if orig, _, ok := st.inbound(*m.InReplyTo); ok {
			if err := st.shim.opt.OnReply(ctx, st, orig, m); err != nil {
				st.shim.log.Warn("shim: reply hook", "agent", st.agent.ID(), "message_id", m.MessageID, "err", err)
			}
		}
	}
	d, ok := st.shim.descriptorByID(toAgentID)
	if !ok {
		st.outbound(m, toAgentID, adapter.Unreachable, ReasonNoSuchAgent, true)
		return adapter.Unreachable, errors.New(ReasonNoSuchAgent)
	}
	m.To = d.Name
	st.outbound(m, d.AgentID, journal.StateSubmitted, "", true)
	state, reason, taskID := st.viaTransport(ctx, d, m)
	st.outboundTask(m, d.AgentID, state, reason, false, taskID)
	if reason != "" {
		return state, errors.New(reason)
	}
	return state, nil
}

func (s *Shim) Mailbox(ctx context.Context, of *mcpserver.Agent, after string, limit int) ([]mcpserver.MailboxEntry, error) {
	st, err := s.state(of)
	if err != nil {
		return nil, err
	}
	es, err := st.rcv.Mailbox(after, limit)
	if errors.Is(err, receiver.ErrUnknownCursor) {
		return nil, &mcpserver.Error{Code: mcpserver.CodeInvalid, Reason: "after"}
	}
	if err != nil {
		return nil, err
	}
	out := make([]mcpserver.MailboxEntry, 0, len(es))
	for _, e := range es {
		me := mcpserver.MailboxEntry{Direction: e.Direction, MessageID: e.MessageID, TaskID: e.TaskID, InReplyTo: e.InReplyTo, From: e.From, To: e.To, Reason: e.Reason, State: adapter.State(e.State), InstanceID: e.InstanceID, SessionRef: e.SessionRef}
		if e.Message != nil {
			me.Text = e.Message.Text()
		}
		out = append(out, me)
	}
	return out, nil
}
