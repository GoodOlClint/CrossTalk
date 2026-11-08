package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

// Request headers a sending host puts its agent's attested fields in. The
// gateway reads them, validates them with a2a.ValidateAttested and discards
// them; the loopback hop carries the X-CrossTalk-GW-* names instead.
const (
	RemoteAgentIDHeader = "X-CrossTalk-Remote-Agent-ID"
	RemoteNameHeader    = "X-CrossTalk-Remote-Name"
	RemoteHarnessHeader = "X-CrossTalk-Remote-Harness"
	RemoteRoleHeader    = "X-CrossTalk-Remote-Role"
)

// ReasonNoSuchAgent is the refusal for a path agent_id that is not a
// registered agent of this host.
const ReasonNoSuchAgent = "no_such_agent"

// ReasonResponseTooLarge: the loopback server answered, but its reply is over
// the forwarding cap; the sender reads it as unknown, never as a cut-off result.
const ReasonResponseTooLarge = "response_too_large"

const (
	forwardPrefix   = "/v1/a2a/"
	maxForwardReply = 1 << 20
	methodSend      = "SendMessage"
	methodGet       = "GetTask"
)

var targetRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Forwarder is the gateway Handler for POST /v1/a2a/{agent_id}.
type Forwarder struct {
	stateDir string
	agents   func(agentID string) (string, bool)
	client   *http.Client
}

// NewForwarder returns a Forwarder that reads the gateway token from stateDir
// and resolves a registered agent_id to its loopback A2A URL with agents.
func NewForwarder(stateDir string, agents func(agentID string) (a2aURL string, ok bool)) *Forwarder {
	return &Forwarder{
		stateDir: stateDir, agents: agents,
		client: &http.Client{
			Timeout:       28 * time.Second,
			Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, ok := PeerFrom(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), forwardPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	reject := func(err error) { writeRPCError(w, r, rpcID(body), err) }

	agentID, encName, ok := attestedFrom(r.Header)
	if !ok {
		reject(a2a.Rejection("", a2a.ReasonRemoteSender))
		return
	}
	req, err := inspect(body)
	if err != nil {
		reject(err)
		return
	}
	target, ok := f.target(rest)
	if !ok {
		reject(a2a.Rejection(req.messageID, ReasonNoSuchAgent))
		return
	}
	tok, err := EnsureToken(f.stateDir)
	if err != nil {
		http.Error(w, "gateway credential unavailable", http.StatusInternalServerError)
		return
	}

	out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	out.Header.Set("Content-Type", "application/json")
	if v := r.Header.Values(sdk.SvcParamVersion); len(v) > 0 {
		out.Header[http.CanonicalHeaderKey(sdk.SvcParamVersion)] = append([]string(nil), v...)
	}
	out.Header.Set("Authorization", "Bearer "+tok)
	out.Header.Set(a2a.GWHostHeader, peer.Fingerprint)
	out.Header.Set(a2a.GWAliasHeader, peer.Alias)
	out.Header.Set(a2a.GWAgentIDHeader, agentID)
	out.Header.Set(a2a.GWNameHeader, encName)
	out.Header.Set(a2a.GWHarnessHeader, r.Header.Get(RemoteHarnessHeader))
	out.Header.Set(a2a.GWRoleHeader, r.Header.Get(RemoteRoleHeader))

	resp, err := f.client.Do(out)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			reject(a2a.Rejection(req.messageID, ReasonNoSuchAgent))
			return
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxForwardReply+1))
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	if len(reply) > maxForwardReply {
		reject(sdk.NewError(sdk.ErrServerError, "response too large").WithDetails(map[string]any{
			a2a.MetaKey: map[string]any{"state": "unknown", "reason": ReasonResponseTooLarge, "message_id": req.messageID},
		}))
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(reply)
}

// attestedFrom validates the sender's Remote-* fields, each present exactly
// once, and returns the agent_id and the base64url name for the rebuilt headers.
func attestedFrom(h http.Header) (agentID, encName string, ok bool) {
	one := func(k string) (string, bool) {
		v := h.Values(k)
		if len(v) != 1 || v[0] == "" {
			return "", false
		}
		return v[0], true
	}
	agentID, ok1 := one(RemoteAgentIDHeader)
	encName, ok2 := one(RemoteNameHeader)
	harness, ok3 := one(RemoteHarnessHeader)
	role, ok4 := one(RemoteRoleHeader)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return "", "", false
	}
	if _, ok := a2a.ValidateAttested(agentID, encName, harness, role); !ok {
		return "", "", false
	}
	return agentID, encName, true
}

// target resolves the path's agent_id in this host's registry. Only an http
// URL on a loopback address is ever dialled.
func (f *Forwarder) target(rest string) (string, bool) {
	if !targetRE.MatchString(rest) {
		return "", false
	}
	raw, ok := f.agents(rest)
	if !ok {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", false
	}
	return u.String(), true
}

type rpcRequest struct{ messageID string }

// inspect applies the method allowlist and the claim refusal to a JSON-RPC
// body. Exactly one top-level key may fold to "method" and at most one to
// "params", so the gateway reads the fields the loopback decoder reads.
func inspect(body []byte) (rpcRequest, error) {
	invalid := sdk.NewError(sdk.ErrInvalidRequest, "invalid request")
	unsupported := sdk.NewError(sdk.ErrUnsupportedOperation, "method not forwarded")
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return rpcRequest{}, invalid
	}
	var methods, params []json.RawMessage
	for dec.More() {
		kt, err := dec.Token()
		key, isStr := kt.(string)
		if err != nil || !isStr {
			return rpcRequest{}, invalid
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return rpcRequest{}, invalid
		}
		switch {
		case strings.EqualFold(key, "method"):
			methods = append(methods, v)
		case strings.EqualFold(key, "params"):
			params = append(params, v)
		}
	}
	if _, err := dec.Token(); err != nil {
		return rpcRequest{}, invalid
	}
	if _, err := dec.Token(); err != io.EOF {
		return rpcRequest{}, invalid
	}
	var method string
	if len(methods) != 1 || json.Unmarshal(methods[0], &method) != nil || (method != methodSend && method != methodGet) || len(params) > 1 {
		return rpcRequest{}, unsupported
	}
	if method == methodGet {
		return rpcRequest{}, nil
	}
	if len(params) == 0 {
		return rpcRequest{}, sdk.NewError(sdk.ErrInvalidParams, "missing params")
	}
	var sr sdk.SendMessageRequest
	if err := json.Unmarshal(params[0], &sr); err != nil || sr.Message == nil {
		return rpcRequest{}, sdk.NewError(sdk.ErrInvalidParams, "invalid params")
	}
	ct, _ := sr.Message.Metadata[a2a.MetaKey].(map[string]any)
	if _, has := ct["claim"]; has {
		return rpcRequest{}, a2a.Rejection(sr.Message.ID, receiver.ReasonRemoteKind)
	}
	return rpcRequest{messageID: sr.Message.ID}, nil
}

// rpcID is the request's JSON-RPC id when it is a string, number or null, and
// null otherwise.
func rpcID(body []byte) json.RawMessage {
	null := json.RawMessage("null")
	var v struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &v) != nil {
		return null
	}
	t := bytes.TrimSpace(v.ID)
	if len(t) > 0 && (t[0] == '"' || t[0] == '-' || (t[0] >= '0' && t[0] <= '9')) {
		return t
	}
	return null
}

type failWith struct {
	a2asrv.PassthroughCallInterceptor
	err error
}

func (f failWith) Before(ctx context.Context, _ *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	return ctx, nil, f.err
}

// writeRPCError answers with err through the SDK's own JSON-RPC encoder, so
// the sender's client reads a gateway refusal exactly as it reads the
// loopback server's: a synthetic request carries the original id into an
// interceptor that fails every call.
func writeRPCError(w http.ResponseWriter, r *http.Request, id json.RawMessage, err error) {
	h := a2asrv.NewJSONRPCHandler(&a2asrv.InterceptedHandler{Handler: a2asrv.NewHandler(nil), Interceptors: []a2asrv.CallInterceptor{failWith{err: err}}})
	syn := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"GetTask","params":{"id":"x"}}`, id)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "/", strings.NewReader(syn))
	req.Header.Set(sdk.SvcParamVersion, string(sdk.Version))
	h.ServeHTTP(w, req)
}
