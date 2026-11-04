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

const (
	ReasonAskClosed      = "ask_closed"
	ReasonAlreadyReplied = "already_replied"
	ReasonLoopCap        = "loop_cap"
)

const (
	hintNativeReachable   = "This session has no CrossTalk binding yet, so it takes no ask or command. send_message with the default kind reaches it; if CrossTalk is installed there, its first CrossTalk call binds it, and then ask works."
	hintNativeUnreachable = "This session has no CrossTalk binding yet and its harness reports it unreachable, so nothing can reach it now. Check list_agents later; if CrossTalk is installed there, its first CrossTalk call binds it, and then ask works."
	hintNativeOneWay      = "Delivered one way to a session with no CrossTalk binding. An answer may never come, or may wait on that session's user."
	hintNoRouteUnbound    = "This message arrived before this session bound, so it has no reply route; nothing expired. Use send_message to the sender."
	hintNoRouteExpired    = "This inform's reply route expired after one hour. Use send_message to the sender."
	hintAmbiguousName     = "Several sessions carry this name. Find the one you mean in list_agents and address it by agent_id."
)

// ErrAskClosed is what Options.OnReply returns when the ask's task reached a
// terminal state before the reply could complete it.
var ErrAskClosed = errors.New("shim: ask is closed")

// AskTransport is a Transport that knows the ask tasks of the agents it
// serves: known reports that messageID is a handle of an ask task, taskID
// names that task, and open that it is not terminal.
type AskTransport interface {
	AskOpen(agentID, messageID string) (taskID string, open, known bool)
}

// AskOrigin is a Transport that resolves any handle of an ask task to the
// task's first message id, the one the asker holds as its outbound ask.
type AskOrigin interface {
	AskMessageID(agentID, handle string) (string, bool)
}

// replyTarget is the in_reply_to and context_id of a reply to orig: for a
// continuation of an ask task, those of the task's original ask, so the
// asker can correlate the reply with its outbound ask.
func (st *AgentState) replyTarget(orig msg.Message) (string, string) {
	ao, ok := st.shim.opt.Transport.(AskOrigin)
	if !ok {
		return orig.MessageID, orig.ContextID
	}
	id, ok := ao.AskMessageID(st.agent.ID(), orig.MessageID)
	if !ok || id == orig.MessageID {
		return orig.MessageID, orig.ContextID
	}
	if first, _, ok := st.inbound(id); ok {
		return id, first.ContextID
	}
	return id, orig.ContextID
}

func askClosed(orig msg.Message) *mcpserver.Error {
	return &mcpserver.Error{Code: mcpserver.CodeRejected, Reason: ReasonAskClosed, Message: "ask is closed; send your answer with send_message to " + orig.From}
}

func (st *AgentState) askTask(messageID string) (taskID string, closed bool) {
	at, ok := st.shim.opt.Transport.(AskTransport)
	if !ok {
		return "", false
	}
	taskID, open, known := at.AskOpen(st.agent.ID(), messageID)
	if !known {
		return "", false
	}
	return taskID, !open
}

// priorReply returns the reply already journaled for the ask task taskID,
// or, without a task, for the inbound messageID, with its obligation's
// latest state.
func (st *AgentState) priorReply(taskID, messageID string) (msg.Message, string, bool) {
	j := st.agent.Journal()
	rs, err := j.Records()
	if err != nil {
		return msg.Message{}, "", false
	}
	for _, rec := range rs {
		if rec.Type != journal.TypeObligation || len(rec.Message) == 0 {
			continue
		}
		if !(taskID != "" && rec.TaskID == taskID) && rec.InReplyTo != messageID {
			continue
		}
		var r msg.Message
		if json.Unmarshal(rec.Message, &r) != nil || r.InReplyTo == nil {
			continue
		}
		l, _ := j.Latest(rec)
		return r, l.State, true
	}
	return msg.Message{}, "", false
}

func (st *AgentState) storedResult(r msg.Message) mcpserver.Result {
	res := mcpserver.Result{MessageID: r.MessageID, State: adapter.Accepted}
	if o, ok := st.agent.Journal().Latest(journal.Record{Type: journal.TypeOutbound, MessageID: r.MessageID}); ok {
		res.State, res.Reason = adapter.State(o.State), o.Reason
	}
	return res
}

func itoa(i int) string { return strconv.Itoa(i) }

func (st *AgentState) newMessage(to string, k msg.Kind, d msg.Delivery, text string) msg.Message {
	id := msg.NewUUIDv7()
	return msg.Message{
		MessageID: id, ContextID: id, From: st.agent.Name(), To: to, Kind: k, Delivery: d,
		HopChain: []string{msg.Hop(st.agent.ID())}, SentAt: st.shim.now(), Parts: []msg.Part{{Type: "text", Text: text}},
	}
}

// withinLoopCap records a send to peer and reports whether it stays within
// the cap. Counts are in memory: a restart resets them.
func (st *AgentState) withinLoopCap(peer string) bool {
	now := st.shim.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.sends[peer][:0:0]
	for _, t := range st.sends[peer] {
		if now.Sub(t) < LoopWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= st.shim.opt.LoopCap {
		st.sends[peer] = kept
		return false
	}
	st.sends[peer] = append(kept, now)
	return true
}

// forwardChain is the hop_chain for a send to target. The send extends the
// chain of the newest A2A message this agent received within ForwardWindow
// that was not a reply, since that message is what a model-made send most
// likely forwards; a send back to the previous hop starts a fresh chain, so
// answering a peer is never a loop, and ping-pong is left to withinLoopCap.
func (st *AgentState) forwardChain(target string) ([]string, error) {
	self := []string{msg.Hop(st.agent.ID())}
	j := st.agent.Journal()
	rs, err := j.Records()
	if err != nil {
		return self, nil
	}
	now := st.shim.now()
	for i := len(rs) - 1; i >= 0; i-- {
		rec := rs[i]
		if rec.Type != journal.TypeDedupe || len(rec.Message) == 0 {
			continue
		}
		var m msg.Message
		if json.Unmarshal(rec.Message, &m) != nil || len(m.HopChain) == 0 || m.InReplyTo != nil || now.Sub(m.SentAt) > ForwardWindow {
			continue
		}
		if l, _ := j.Latest(rec); adapter.State(l.State) == adapter.Rejected {
			continue
		}
		if m.HopChain[len(m.HopChain)-1] == msg.Hop(target) {
			return self, nil
		}
		return msg.AppendHop(m.HopChain, self[0])
	}
	return self, nil
}

func (st *AgentState) outbound(m msg.Message, to string, state adapter.State, reason string, withBody bool) error {
	return st.outboundTask(m, to, state, reason, withBody, "")
}

// outboundTask also keeps the far side's task id, when the Transport reports one.
func (st *AgentState) outboundTask(m msg.Message, to string, state adapter.State, reason string, withBody bool, taskID string) error {
	r := journal.Record{Type: journal.TypeOutbound, MessageID: m.MessageID, To: to, Kind: string(m.Kind), State: string(state), Reason: reason, TaskID: taskID, ContextID: m.ContextID}
	if m.InReplyTo != nil {
		r.InReplyTo = *m.InReplyTo
	}
	if withBody {
		r.Message, _ = json.Marshal(m)
	}
	err := st.agent.Journal().Append(r)
	if err != nil {
		st.shim.log.Warn("shim: journal outbound", "agent", st.agent.ID(), "message_id", m.MessageID, "err", err)
	}
	return err
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

// deliver sends m by name or agent_id: a descriptor means a shim-served agent (Transport);
// otherwise a native session injected through this agent's adapters.
// Every outbound is journaled before and after the attempt.
func (st *AgentState) deliver(ctx context.Context, m msg.Message) mcpserver.Result {
	res := mcpserver.Result{MessageID: m.MessageID}
	finish := func(to string, state adapter.State, reason string) mcpserver.Result {
		st.outbound(m, to, state, reason, false)
		res.State, res.Reason = state, reason
		if reason == ReasonAmbiguousName {
			res.Hint = hintAmbiguousName
		}
		return res
	}
	var fs []found
	discover := func(ctx context.Context) ([]mcpserver.Session, error) {
		var err error
		fs, err = st.shim.sessions(ctx)
		var out []mcpserver.Session
		for _, f := range fs {
			out = append(out, f.s)
		}
		return out, err
	}
	tgt, err := mcpserver.ResolveTarget(ctx, st.shim.reg, discover, m.To)
	if err != nil {
		return finish(m.To, adapter.Unreachable, err.Error())
	}
	if d := tgt.Desc; tgt.Registered {
		if d.AgentID == st.agent.ID() {
			return finish(d.AgentID, adapter.Rejected, ReasonSelf)
		}
		if !st.withinLoopCap(d.AgentID) {
			return finish(d.AgentID, adapter.Rejected, ReasonLoopCap)
		}
		chain, err := st.forwardChain(d.AgentID)
		if err != nil {
			reason := receiver.ReasonHopRepeat
			if errors.Is(err, msg.ErrHopOverflow) {
				reason = receiver.ReasonHopOverflow
			}
			return finish(d.AgentID, adapter.Rejected, reason)
		}
		m.HopChain = chain
		if st.outbound(m, d.AgentID, journal.StateSubmitted, "", true) != nil {
			res.State, res.Reason = adapter.Unreachable, receiver.ReasonJournal
			return res
		}
		state, reason, taskID := st.viaTransport(ctx, d, m)
		st.outboundTask(m, d.AgentID, state, reason, false, taskID)
		res.State, res.Reason = state, reason
		return res
	}
	if m.Kind != msg.Inform {
		switch len(tgt.Native) {
		case 0:
			return finish(m.To, adapter.Rejected, ReasonNoSuchAgent)
		case 1:
		default:
			return finish(m.To, adapter.Rejected, ReasonAmbiguousName)
		}
		res := finish(m.To, adapter.Rejected, ReasonNativeInformOnly)
		res.Hint = hintNativeUnreachable
		if tgt.Native[0].Reachable {
			res.Hint = hintNativeReachable
		}
		return res
	}
	b, ad, state, reason := st.nativeTarget(ctx, tgt.Native, fs)
	if ad == nil {
		return finish(m.To, state, reason)
	}
	to := string(b.Harness) + ":" + b.SessionRef
	if !st.withinLoopCap(to) {
		return finish(to, adapter.Rejected, ReasonLoopCap)
	}
	if st.outbound(m, to, journal.StateSubmitted, "", true) != nil {
		res.State, res.Reason = adapter.Unreachable, receiver.ReasonJournal
		return res
	}
	state, err = ad.Inject(ctx, m, adapter.Delivery{Mode: m.Delivery, Target: b, VerifiedSender: true, SenderHarness: string(st.agent.Harness())})
	if !state.Injectable() {
		state = adapter.Unknown
	}
	reason = ""
	if err != nil {
		reason = err.Error()
	}
	res = finish(to, state, reason)
	if reason == "" && (state == adapter.Accepted || state == adapter.Queued) {
		res.Hint = hintNativeOneWay
	}
	return res
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
		if s := adapter.State(latest[id].State); s == adapter.Rejected || s == adapter.Unreachable || st.replied(id) || st.noticed(id) || st.legacyReceipt(ask, latest[id].To) {
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
	if !st.agent.Admit() {
		return
	}
	defer st.agent.Done()
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

// replied reports whether a reply receipt exists for the outbound ask
// messageID: an admitted reply from the asked peer in the ask's context.
func (st *AgentState) replied(messageID string) bool {
	rs, err := st.agent.Journal().Records()
	if err != nil {
		return false
	}
	for _, r := range rs {
		if r.Type == journal.TypeReceipt && r.InReplyTo == messageID {
			return true
		}
	}
	return false
}

// noticed reports whether this agent's session was already told that the
// ask messageID expired.
func (st *AgentState) noticed(messageID string) bool {
	rs, err := st.agent.Journal().Records()
	if err != nil {
		return false
	}
	for _, r := range rs {
		var m msg.Message
		if r.Type == journal.TypeDedupe && r.Sender == PrincipalCrossTalk && len(r.Message) > 0 && json.Unmarshal(r.Message, &m) == nil && m.InReplyTo != nil && *m.InReplyTo == messageID {
			return true
		}
	}
	return false
}

// legacyReceipt accepts, for a journal written before reply receipts, an
// admitted inbound body from peer answering ask in its context, and records
// it as a receipt so the fallback runs once.
func (st *AgentState) legacyReceipt(ask msg.Message, peer string) bool {
	j := st.agent.Journal()
	rs, err := j.Records()
	if err != nil || !mcpserver.IsAgentID(peer) {
		return false
	}
	for _, r := range rs {
		if r.Type != journal.TypeDedupe || r.Sender != peer || len(r.Message) == 0 {
			continue
		}
		var m msg.Message
		if json.Unmarshal(r.Message, &m) != nil || m.InReplyTo == nil || *m.InReplyTo != ask.MessageID || m.ContextID != ask.ContextID {
			continue
		}
		if l, ok := j.Latest(r); !ok || !receiver.Admitted(l.State) {
			continue
		}
		return j.Append(journal.Record{Type: journal.TypeReceipt, MessageID: m.MessageID, InReplyTo: ask.MessageID, Sender: peer, ContextID: ask.ContextID}) == nil
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

// Reply answers an inbound message; the destination is the stored original's
// verified sender, never the model's text (§8, §5 F4). There is one reply
// per ask task (any of its message ids) or, without a task, per inbound
// message (ADR 0010): a byte-identical retry returns the first result, or
// resumes the reply transaction while its obligation is not yet delivered.
func (s *Shim) Reply(ctx context.Context, from *mcpserver.Agent, messageID, text string) (mcpserver.Result, error) {
	st, err := s.state(from)
	if err != nil {
		return mcpserver.Result{}, err
	}
	st.replyMu.Lock()
	defer st.replyMu.Unlock()
	orig, key, ok := st.inbound(messageID)
	if !ok {
		return mcpserver.Result{}, &mcpserver.Error{Code: mcpserver.CodeInvalid, Reason: ReasonNoSuchMessage, Hint: hintNoRouteUnbound}
	}
	if key == receiver.LabelUnverified {
		return mcpserver.Result{}, &mcpserver.Error{Code: string(adapter.Unreachable), Reason: ReasonSenderUnverified}
	}
	taskID, closed := st.askTask(orig.MessageID)
	if prev, state, ok := st.priorReply(taskID, orig.MessageID); ok {
		switch {
		case state == journal.StateClosed || prev.Text() != text && closed:
			return mcpserver.Result{}, askClosed(orig)
		case prev.Text() != text:
			return mcpserver.Result{}, &mcpserver.Error{Code: mcpserver.CodeRejected, Reason: ReasonAlreadyReplied, Message: "this ask already has a different reply " + prev.MessageID + "; send further answers with send_message to " + orig.From}
		case state != journal.StateDelivered:
			return st.completeReply(ctx, orig, prev, key, taskID, state)
		}
		return st.storedResult(prev), nil
	}
	if closed {
		return mcpserver.Result{}, askClosed(orig)
	}
	if mcpserver.IsAgentID(key) && orig.Kind != msg.Ask && !st.rcv.RouteOpen(orig.MessageID) {
		return mcpserver.Result{}, &mcpserver.Error{Code: mcpserver.CodeInvalid, Reason: ReasonNoSuchMessage, Hint: hintNoRouteExpired}
	}
	r := st.newMessage(orig.From, msg.Inform, msg.Immediate, text)
	irt, ctxID := st.replyTarget(orig)
	r.InReplyTo, r.ContextID = &irt, ctxID
	if err := st.agent.Journal().Append(st.obligation(r, key, journal.StatePending, true, taskID)); err != nil {
		return mcpserver.Result{}, err
	}
	return st.completeReply(ctx, orig, r, key, taskID, journal.StatePending)
}

// Obligation stages after pending (journal.StatePending): the ask's task is
// completed, then the route is released, then the reply is delivered.
const (
	stageCompleted = journal.StateCompleted
	stageReleased  = "released"
)

// completeReply runs the reply transaction from the obligation's journaled
// stage. Each stage is journaled before the next starts, so a failed stage
// leaves the obligation where a retry or startup replay resumes it.
func (st *AgentState) completeReply(ctx context.Context, orig, r msg.Message, key, taskID, stage string) (mcpserver.Result, error) {
	res := mcpserver.Result{MessageID: r.MessageID}
	advance := func(s string) bool {
		if err := st.agent.Journal().Append(st.obligation(r, key, s, false, taskID)); err != nil {
			res.State, res.Reason = adapter.Unknown, receiver.ReasonJournal
			return false
		}
		stage = s
		return true
	}
	if stage == journal.StatePending {
		hook := st.shim.opt.OnReply
		if hook == nil && !mcpserver.IsAgentID(key) {
			res.State, res.Reason = adapter.Unreachable, ReasonNoReplyHook
			return res, nil
		}
		if hook != nil {
			err := hook(ctx, st, orig, r)
			if errors.Is(err, ErrAskClosed) {
				st.agent.Journal().Append(st.obligation(r, key, journal.StateClosed, false, taskID))
				return mcpserver.Result{}, askClosed(orig)
			}
			if err != nil {
				res.State, res.Reason = adapter.Unknown, err.Error()
				return res, nil
			}
		}
		if !advance(stageCompleted) {
			return res, nil
		}
	}
	if stage == stageCompleted {
		st.rcv.CloseRoute(orig.MessageID)
		if !advance(stageReleased) {
			return res, nil
		}
	}
	if !mcpserver.IsAgentID(key) {
		if advance(journal.StateDelivered) {
			res.State = adapter.Accepted
		}
		return res, nil
	}
	return st.forwardReply(ctx, r, key, taskID), nil
}

func (st *AgentState) obligation(r msg.Message, to, state string, withBody bool, taskID string) journal.Record {
	rec := journal.Record{Type: journal.TypeObligation, MessageID: r.MessageID, InReplyTo: *r.InReplyTo, To: to, State: state, TaskID: taskID}
	if withBody {
		rec.Message, _ = json.Marshal(r)
	}
	return rec
}

// forwardReply sends r to the asker. An outbound record already accepted or
// queued is the forward's evidence: it is not sent again.
func (st *AgentState) forwardReply(ctx context.Context, r msg.Message, key, askTask string) mcpserver.Result {
	res := mcpserver.Result{MessageID: r.MessageID}
	delivered := func() {
		st.agent.Journal().Append(st.obligation(r, key, journal.StateDelivered, false, askTask))
	}
	if o, ok := st.agent.Journal().Latest(journal.Record{Type: journal.TypeOutbound, MessageID: r.MessageID}); ok && (o.State == string(adapter.Accepted) || o.State == string(adapter.Queued)) {
		delivered()
		res.State, res.Reason = adapter.State(o.State), o.Reason
		return res
	}
	d, ok := st.shim.descriptorByID(key)
	if !ok {
		res.State, res.Reason = adapter.Unreachable, ReasonNoSuchAgent
		st.outbound(r, key, res.State, res.Reason, false)
		return res
	}
	r.To = d.Name
	if st.outbound(r, d.AgentID, journal.StateSubmitted, "", true) != nil {
		res.State, res.Reason = adapter.Unreachable, receiver.ReasonJournal
		return res
	}
	var taskID string
	res.State, res.Reason, taskID = st.viaTransport(ctx, d, r)
	st.outboundTask(r, d.AgentID, res.State, res.Reason, false, taskID)
	if res.State == adapter.Accepted || res.State == adapter.Queued {
		delivered()
	}
	return res
}

// retryObligations resumes every reply obligation of this conversation that
// is neither delivered nor closed, local-user ones included, from its
// journaled stage. It runs once the agent's descriptor is published, so the
// asker can resolve this agent; the receiver dedupes on message_id, so a
// resend of a reply that did arrive is not injected twice (C4).
func (st *AgentState) retryObligations() {
	st.replyMu.Lock()
	defer st.replyMu.Unlock()
	j := st.agent.Journal()
	rs, err := j.Records()
	if err != nil {
		return
	}
	cur := journal.Binding{HarnessEndpoint: st.binding.HarnessEndpoint, SessionRef: st.binding.SessionRef}
	for _, rec := range rs {
		if rec.Type != journal.TypeObligation || len(rec.Message) == 0 || !rec.Binding.SameConversation(cur) {
			continue
		}
		l, ok := j.Latest(rec)
		if !ok || l.State == journal.StateDelivered || l.State == journal.StateClosed {
			continue
		}
		var r msg.Message
		if json.Unmarshal(rec.Message, &r) != nil || r.InReplyTo == nil {
			continue
		}
		if st.ctx.Err() != nil {
			return
		}
		orig, _, ok := st.inbound(*r.InReplyTo)
		if !ok {
			orig = msg.Message{MessageID: *r.InReplyTo, From: r.To}
		}
		st.completeReply(st.ctx, orig, r, rec.To, rec.TaskID, l.State)
	}
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
