package shim

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

const (
	ReasonUnknownAlias      = "unknown_alias"
	ReasonRemoteNative      = "remote_native_unsupported"
	ReasonUnpairedByPeer    = remote.ReasonUnpairedByPeer
	ReasonRemoteKind        = receiver.ReasonRemoteKind
	ReasonPending           = "pending"
	hintCollisionUnknown    = "%s could name a session on %s as well as one on this host, and %s did not answer, so nothing was sent. Address the local session by its agent_id %s, or try again when the host answers."
	hintAmbiguousRemoteName = "%s names a session on this host (agent_id %s) and a session on %s. Address the local one as %s, the remote one as %s@%s."
)

// RemoteTransport is a Transport that can send to an agent on a paired host.
// The agent is named by agent_id alone; the returned error text is a reason code.
type RemoteTransport interface {
	SendRemote(ctx context.Context, from registry.Descriptor, host remote.Host, peer remote.Peer, agentID string, m msg.Message) (taskID string, state adapter.State, err error)
}

// IsPairedAlias reports whether alias is in this host's peers.json.
func (s *Shim) IsPairedAlias(alias string) bool {
	_, err := s.peerByAlias(alias)
	return err == nil
}

var errNoPeer = errors.New("shim: no such paired host")

func (s *Shim) peerByAlias(alias string) (remote.Peer, error) {
	dir, err := s.remoteDir()
	if err != nil {
		return remote.Peer{}, err
	}
	ps, err := remote.NewPeers(dir).List()
	if err != nil {
		return remote.Peer{}, err
	}
	for _, p := range ps {
		if p.Alias == alias {
			return p, nil
		}
	}
	return remote.Peer{}, errNoPeer
}

// peerRows fetches alias's listing under the shared remote fetch bound.
func (s *Shim) peerRows(ctx context.Context, alias string) (remote.Host, remote.Peer, []remote.AgentRow, error) {
	p, err := s.peerByAlias(alias)
	if err != nil {
		return remote.Host{}, p, nil, err
	}
	dir, _ := s.remoteDir()
	h, err := remote.LoadHost(dir)
	if err != nil {
		return h, p, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, RemoteFetchBudget)
	defer cancel()
	rows, err := remote.NewClient(h, p).Agents(ctx)
	return h, p, rows, err
}

// pickRemote selects the one messageable agent ref names among rows: by
// agent_id when ref has that shape, else by exact name. A reason means none.
func pickRemote(rows []remote.AgentRow, ref string) (remote.AgentRow, string) {
	if mcpserver.IsAgentID(ref) {
		for _, r := range rows {
			if r.AgentID == ref {
				if !r.CrossTalk {
					return r, ReasonRemoteNative
				}
				return r, ""
			}
		}
		return remote.AgentRow{}, ReasonNoSuchAgent
	}
	var reg []remote.AgentRow
	natives := 0
	for _, r := range rows {
		switch {
		case r.Name != ref:
		case r.CrossTalk:
			reg = append(reg, r)
		default:
			natives++
		}
	}
	switch {
	case len(reg) == 1:
		return reg[0], ""
	case len(reg) > 1:
		return remote.AgentRow{}, ReasonAmbiguousName
	case natives > 0:
		return remote.AgentRow{}, ReasonRemoteNative
	}
	return remote.AgentRow{}, ReasonNoSuchAgent
}

// stallHint is the handshake-stall text for err, built from local data only,
// or "" when err is not a stalled handshake.
func stallHint(err error) string {
	var he *remote.HandshakeError
	if errors.As(err, &he) {
		return he.Error()
	}
	return ""
}

func listingFailure(err error) (adapter.State, string) {
	var re *remote.Error
	if errors.As(err, &re) {
		if re.Reason == remote.ReasonUnpairedByPeer {
			return adapter.Rejected, re.Reason
		}
		return adapter.Unreachable, re.Reason
	}
	return adapter.Unreachable, remote.ReasonUnreachable
}

// CheckRemote reports whether the paired host rr.Alias lists a session the
// value rr also reaches, and its agent_id. A failed lookup is an error: the
// caller cannot rule a collision out.
func (s *Shim) CheckRemote(ctx context.Context, rr mcpserver.RemoteRef) (string, bool, error) {
	_, _, rows, err := s.peerRows(ctx, rr.Alias)
	if err != nil {
		return "", false, err
	}
	for _, r := range rows {
		if mcpserver.IsAgentID(rr.Ref) && r.AgentID == rr.Ref || !mcpserver.IsAgentID(rr.Ref) && r.Name == rr.Ref {
			return r.AgentID, true, nil
		}
	}
	return "", false, nil
}

func (st *AgentState) localAgentID(tgt mcpserver.Target) string {
	if tgt.Registered {
		return tgt.Desc.AgentID
	}
	if len(tgt.Native) > 0 {
		return identity.AgentID(string(tgt.Native[0].Harness), tgt.Native[0].SessionRef)
	}
	return ""
}

// deliverRemote sends m to rr on a paired host (design §8). finish journals
// and returns a refusal that made no network call for the send itself.
func (st *AgentState) deliverRemote(ctx context.Context, m msg.Message, rr *mcpserver.RemoteRef, finish func(string, adapter.State, string) mcpserver.Result) mcpserver.Result {
	if m.Kind != msg.Inform && m.Kind != msg.Ask {
		res := finish(m.To, adapter.Rejected, ReasonRemoteKind)
		res.Hint = mcpserver.HintRemoteKind
		return res
	}
	host, peer, rows, err := st.shim.peerRows(ctx, rr.Alias)
	if err != nil {
		state, reason := listingFailure(err)
		res := finish(m.To, state, reason)
		res.Hint = stallHint(err)
		return res
	}
	row, reason := pickRemote(rows, rr.Ref)
	switch reason {
	case "":
	case ReasonAmbiguousName:
		res := finish(m.To, adapter.Rejected, reason)
		res.Hint = fmt.Sprintf("Several sessions on %s carry this name. Find the one you mean in list_agents and address it as agent_id@%s.", rr.Alias, rr.Alias)
		return res
	default:
		return finish(m.To, adapter.Rejected, reason)
	}
	rt, ok := st.shim.opt.Transport.(RemoteTransport)
	if !ok {
		return finish(m.To, adapter.Unreachable, ReasonNoTransport)
	}
	key := receiver.RemoteKey(peer.Fingerprint, row.AgentID)
	res := mcpserver.Result{MessageID: m.MessageID}
	if st.outbound(m, key, journal.StateSubmitted, "", true) != nil {
		res.State, res.Reason = adapter.Unreachable, receiver.ReasonJournal
		return res
	}
	taskID, state, err := rt.SendRemote(ctx, st.agent.Descriptor(), host, peer, row.AgentID, m)
	reason = ""
	if err != nil {
		reason = err.Error()
	}
	if !state.Injectable() {
		state = adapter.Unknown
	}
	st.outboundTask(m, key, state, reason, false, taskID)
	res.State, res.Reason = state, reason
	res.Hint = joinHint(stallHint(err), st.countSend(key, state))
	return res
}

// bareNameHint lists the name@host rows of paired hosts that carry the bare
// name to, so the model can retry with the full address. Empty when there are
// none; to is never sent anywhere.
func (st *AgentState) bareNameHint(ctx context.Context, to string) string {
	if to == "" || mcpserver.IsAgentID(to) {
		return ""
	}
	if _, _, ok := mcpserver.SplitAddress(to); ok {
		return ""
	}
	var matches []string
	for _, r := range st.shim.ListRemote(ctx).Rows {
		if r.Name == to+"@"+r.Host && r.CrossTalk {
			matches = append(matches, r.Name)
		}
	}
	if len(matches) == 0 {
		return ""
	}
	return "No session on this host has that name, but a paired host does: address it as " + joinNames(matches) + "."
}

func joinNames(ns []string) string {
	out := ""
	for i, n := range ns {
		if i > 0 {
			out += " or "
		}
		out += n
	}
	return out
}
