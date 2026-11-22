package codex

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

var _ harness.Refreshable = (*Harness)(nil)

type refreshState struct {
	Claims []refreshClaim `json:"claims"`
}

type refreshClaim struct {
	Claim Claim `json:"claim"`
	Live  bool  `json:"live"`
}

// RefreshState carries each proven claim; the thread's lock and the shim's
// parent are not carried, the new process proves them again.
func (h *Harness) RefreshState() json.RawMessage {
	rt := h.wakeRoute()
	if rt == nil {
		return nil
	}
	var st refreshState
	for _, b := range rt.claims.Snapshot() {
		st.Claims = append(st.Claims, refreshClaim{Claim: b.Claim, Live: b.Live})
	}
	if len(st.Claims) == 0 {
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return nil
	}
	return b
}

// ResumeState keeps the carried claims for ServeClaims to prove again and
// returns their threads, which ServeClaims registers after the proof.
func (h *Harness) ResumeState(raw json.RawMessage) []string {
	var st refreshState
	if json.Unmarshal(raw, &st) != nil {
		return nil
	}
	var bs []Binding
	var threads []string
	for _, c := range st.Claims {
		bs = append(bs, Binding{Claim: c.Claim, Live: c.Live})
		threads = append(threads, c.Claim.SessionID)
	}
	h.mu.Lock()
	h.resume = bs
	h.mu.Unlock()
	return threads
}

func (h *Harness) takeResume() []Binding {
	h.mu.Lock()
	defer h.mu.Unlock()
	bs := h.resume
	h.resume = nil
	return bs
}

// dropResumed retires the agents of carried claims that ServeClaims could not
// prove because it never started (setting off, parent not a Codex process).
func dropResumed(ctx context.Context, r harness.SessionRegistrar, bs []Binding, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	for _, b := range bs {
		req := harness.RequestContext{Meta: map[string]any{"threadId": b.Claim.SessionID}}
		retired, err := r.RetireSession(ctx, req)
		log.Info("codex claim dropped on refresh", "thread", b.Claim.SessionID, "err", "claim source not running", "retired", retired, "retire_err", err)
	}
}
