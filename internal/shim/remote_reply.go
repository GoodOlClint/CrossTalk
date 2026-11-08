package shim

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

const (
	DefaultReplyRetryFirst    = 60 * time.Second
	DefaultReplyRetryCap      = 10 * time.Minute
	DefaultReplyNoDeadlineMax = 24 * time.Hour
	ReasonReplyExpired        = "reply_expired"
)

func (s *Shim) replyRetry() ReplyRetry {
	rp := s.opt.ReplyRetry
	if rp.First <= 0 {
		rp.First = DefaultReplyRetryFirst
	}
	if rp.Cap <= 0 {
		rp.Cap = DefaultReplyRetryCap
	}
	if rp.NoDeadline <= 0 {
		rp.NoDeadline = DefaultReplyNoDeadlineMax
	}
	if rp.After == nil {
		rp.After = time.After
	}
	return rp
}

func (s *Shim) peerByFingerprint(fp string) (remote.Peer, error) {
	dir, err := s.remoteDir()
	if err != nil {
		return remote.Peer{}, err
	}
	ps, err := remote.NewPeers(dir).List()
	if err != nil {
		return remote.Peer{}, err
	}
	for _, p := range ps {
		if p.Fingerprint == fp {
			return p, nil
		}
	}
	return remote.Peer{}, errNoPeer
}

func splitRemoteKey(key string) (fp, agentID string, ok bool) {
	rest, ok := strings.CutPrefix(key, receiver.RemoteKeyPrefix)
	if !ok {
		return "", "", false
	}
	fp, agentID, ok = strings.Cut(rest, "/")
	return fp, agentID, ok && fp != "" && mcpserver.IsAgentID(agentID)
}

// askReceivedAt is when this host journaled the inbound message id, on this
// host's clock.
func (st *AgentState) askReceivedAt(id string) (time.Time, bool) {
	rs, err := st.agent.Journal().Records()
	if err != nil {
		return time.Time{}, false
	}
	for _, r := range rs {
		if r.Type == journal.TypeDedupe && r.MessageID == id {
			return r.At, true
		}
	}
	return time.Time{}, false
}

func (st *AgentState) endReply(r msg.Message, key, taskID, state, reason string) mcpserver.Result {
	rec := st.obligation(r, key, state, false, taskID)
	rec.Reason = reason
	st.agent.Journal().Append(rec)
	return mcpserver.Result{MessageID: r.MessageID, State: adapter.State(state), Reason: reason}
}

// forwardRemoteReply posts r to the asker on the paired host key names: the
// peer is found by fingerprint and the asker by agent_id, never by label or
// name. A gateway answer other than accepted or queued is not delivery.
func (st *AgentState) forwardRemoteReply(ctx context.Context, orig, r msg.Message, key, taskID string) mcpserver.Result {
	res := mcpserver.Result{MessageID: r.MessageID}
	if o, ok := st.agent.Journal().Latest(journal.Record{Type: journal.TypeOutbound, MessageID: r.MessageID}); ok && (o.State == string(adapter.Accepted) || o.State == string(adapter.Queued)) {
		st.agent.Journal().Append(st.obligation(r, key, journal.StateDelivered, false, taskID))
		res.State, res.Reason = adapter.State(o.State), o.Reason
		return res
	}
	pending := func(state adapter.State) mcpserver.Result {
		res.State, res.Reason = state, ReasonRemoteReplyPending
		st.armReplyRetry(r.MessageID)
		return res
	}
	rp := st.shim.replyRetry()
	limit := rp.NoDeadline
	if orig.DeadlineSeconds > 0 {
		limit = time.Duration(orig.DeadlineSeconds) * time.Second
	}
	if at, ok := st.askReceivedAt(orig.MessageID); ok && st.shim.now().After(at.Add(limit)) {
		return st.endReply(r, key, taskID, ObligationUnreachable, ReasonReplyExpired)
	}
	fp, agentID, ok := splitRemoteKey(key)
	rt, isRT := st.shim.opt.Transport.(RemoteTransport)
	if !ok || !isRT {
		return pending(adapter.Unreachable)
	}
	peer, err := st.shim.peerByFingerprint(fp)
	if err != nil {
		return pending(adapter.Unreachable)
	}
	dir, _ := st.shim.remoteDir()
	host, err := remote.LoadHost(dir)
	if err != nil {
		return pending(adapter.Unreachable)
	}
	if st.outbound(r, key, journal.StateSubmitted, "", true) != nil {
		res.State, res.Reason = adapter.Unreachable, receiver.ReasonJournal
		return res
	}
	tid, state, err := rt.SendRemote(ctx, st.agent.Descriptor(), host, peer, agentID, r)
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	if !state.Injectable() {
		state = adapter.Unknown
	}
	st.outboundTask(r, key, state, reason, false, tid)
	switch state {
	case adapter.Accepted, adapter.Queued:
		st.agent.Journal().Append(st.obligation(r, key, journal.StateDelivered, false, taskID))
		res.State, res.Reason = state, reason
		return res
	case adapter.Rejected:
		if reason == "" {
			reason = string(adapter.Rejected)
		}
		return st.endReply(r, key, taskID, ObligationRejected, reason)
	}
	return pending(state)
}

// armReplyRetry starts the timer that resends the reply messageID while the
// agent lives; one timer runs per reply.
func (st *AgentState) armReplyRetry(messageID string) {
	st.mu.Lock()
	if st.closed || st.retrying[messageID] {
		st.mu.Unlock()
		return
	}
	if st.retrying == nil {
		st.retrying = map[string]bool{}
	}
	st.retrying[messageID] = true
	st.mu.Unlock()
	go func() {
		defer func() {
			st.mu.Lock()
			delete(st.retrying, messageID)
			st.mu.Unlock()
		}()
		rp := st.shim.replyRetry()
		delay := rp.First
		for {
			select {
			case <-st.ctx.Done():
				return
			case <-rp.After(delay):
			}
			if !st.agent.Admit() {
				return
			}
			done := st.retryReply(messageID)
			st.agent.Done()
			if done {
				return
			}
			delay = min(delay*2, rp.Cap)
		}
	}()
}

// retryReply resumes the obligation of reply messageID once; it reports
// whether the obligation has ended.
func (st *AgentState) retryReply(messageID string) bool {
	st.replyMu.Lock()
	defer st.replyMu.Unlock()
	j := st.agent.Journal()
	rs, err := j.Records()
	if err != nil {
		return false
	}
	for _, rec := range rs {
		if rec.Type != journal.TypeObligation || rec.MessageID != messageID || len(rec.Message) == 0 {
			continue
		}
		l, ok := j.Latest(rec)
		if !ok || obligationEnded(l.State) {
			return true
		}
		var r msg.Message
		if json.Unmarshal(rec.Message, &r) != nil || r.InReplyTo == nil {
			return true
		}
		orig, _, ok := st.inbound(*r.InReplyTo)
		if !ok {
			orig = msg.Message{MessageID: *r.InReplyTo, From: r.To}
		}
		st.completeReply(st.ctx, orig, r, rec.To, rec.TaskID, l.State)
		l, _ = j.Latest(rec)
		return obligationEnded(l.State)
	}
	return true
}

func obligationEnded(state string) bool {
	switch state {
	case journal.StateDelivered, journal.StateClosed, ObligationRejected, ObligationUnreachable:
		return true
	}
	return false
}
