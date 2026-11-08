package wire

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"sync/atomic"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

var reasonRE = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// SendRemote posts m to agentID on peer's gateway, pinned to the peer's
// fingerprint, then polls GetTask at the same agent_id path for the settle
// bound. The path carries the agent_id only; the sender's fields travel as
// the gateway's Remote-* headers.
func (w *Wire) SendRemote(ctx context.Context, from registry.Descriptor, host remote.Host, peer remote.Peer, agentID string, m msg.Message) (string, adapter.State, error) {
	cl := remote.NewClient(host, peer)
	hdr := http.Header{}
	hdr.Set(remote.RemoteAgentIDHeader, from.AgentID)
	hdr.Set(remote.RemoteNameHeader, base64.RawURLEncoding.EncodeToString([]byte(from.Name)))
	hdr.Set(remote.RemoteHarnessHeader, from.Harness)
	hdr.Set(remote.RemoteRoleHeader, from.Role)
	c := &a2a.Client{HTTP: cl.HTTPClient(), Headers: hdr}
	url := "https://" + peer.Address + "/v1/a2a/" + agentID

	var wrote atomic.Bool
	sctx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }})
	id, state, err := c.Send(sctx, url, "", m, true)
	if err != nil {
		state, err = sendFailure(err, state, wrote.Load())
		return id, state, err
	}
	if !settling(state) || id == "" {
		return id, state, nil
	}
	id, state, err = pollSettled(ctx, c, url, "", id, RemoteSettle)
	if err != nil && state != adapter.Unknown {
		return id, state, errors.New(safeReason(err))
	}
	return id, state, err
}

func safeReason(err error) string {
	if r := err.Error(); reasonRE.MatchString(r) {
		return r
	}
	return "rejected"
}

// sendFailure maps a failed SendMessage to a state and reason. A protocol
// refusal from the gateway keeps its state; a transport failure is unreachable
// when the request never left, unknown/pending when it may have been
// forwarded, and a refused handshake is the peer unpairing this host.
func sendFailure(err error, state adapter.State, wrote bool) (adapter.State, error) {
	var ae *sdk.Error
	if errors.As(err, &ae) {
		return state, errors.New(safeReason(errors.New(remoteReason(err))))
	}
	switch r := remote.Classify(err); r {
	case remote.ReasonUnpairedByPeer:
		return adapter.Rejected, errors.New(r)
	case remote.ReasonPinMismatch, remote.ReasonGatewayNotRunning:
		return adapter.Unreachable, errors.New(r)
	default:
		if wrote {
			return adapter.Unknown, errors.New(ReasonPending)
		}
		return adapter.Unreachable, errors.New(r)
	}
}
