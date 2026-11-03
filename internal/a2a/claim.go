package a2a

import (
	"context"
	"errors"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

// ReasonClaimSender: a claim must come from a registered agent naming itself
// as the owner.
const ReasonClaimSender = "claim_sender"

// ClaimFunc records (claimed) or ends the sender's claim on the served agent
// (§7). A *ClaimError return reaches the client with its state and reason.
type ClaimFunc func(ctx context.Context, sender receiver.Sender, claimed bool) error

// ClaimError is a refused or failed claim as both sides see it.
type ClaimError struct {
	State  adapter.State
	Reason string
}

func (e *ClaimError) Error() string { return "a2a: claim " + string(e.State) + ": " + e.Reason }

type claimRequest struct {
	owner   string
	claimed bool
}

// claimOf reads metadata.crosstalk.claim: {owner, claimed}.
func claimOf(am *sdk.Message) (claimRequest, bool, error) {
	ct, _ := am.Metadata[MetaKey].(map[string]any)
	raw, ok := ct["claim"]
	if !ok || raw == nil {
		return claimRequest{}, false, nil
	}
	c, ok := raw.(map[string]any)
	if !ok {
		return claimRequest{}, true, badMessage("metadata.%s.claim is not an object", MetaKey)
	}
	owner, ok1 := c["owner"].(string)
	claimed, ok2 := c["claimed"].(bool)
	if !ok1 || !ok2 {
		return claimRequest{}, true, badMessage("metadata.%s.claim needs owner and claimed", MetaKey)
	}
	return claimRequest{owner: owner, claimed: claimed}, true, nil
}

func claimFailure(state adapter.State, reason string) error {
	base := sdk.ErrServerError
	if state == adapter.Rejected {
		base = sdk.ErrUnauthorized
	}
	return sdk.NewError(base, string(state)+": "+reason).WithDetails(map[string]any{
		MetaKey: map[string]any{"state": string(state), "reason": reason},
	})
}

// claim never creates a task or reaches the receiver chain.
func (s *Server) claim(ctx context.Context, snd receiver.Sender, c claimRequest) (sdk.SendMessageResult, error) {
	if s.cfg.Claim == nil {
		return nil, sdk.ErrUnsupportedOperation
	}
	if snd.AgentID == "" || c.owner != snd.Name {
		return nil, claimFailure(adapter.Rejected, ReasonClaimSender)
	}
	if err := s.cfg.Claim(ctx, snd, c.claimed); err != nil {
		var ce *ClaimError
		if errors.As(err, &ce) {
			return nil, claimFailure(ce.State, ce.Reason)
		}
		return nil, claimFailure(adapter.Unknown, "claim could not be recorded")
	}
	text := "released"
	if c.claimed {
		text = "claimed"
	}
	return &sdk.Message{ID: msg.NewUUIDv7(), Role: sdk.MessageRoleAgent, Parts: sdk.ContentParts{sdk.NewTextPart(text)}}, nil
}

// Claim asks the agent at url to record (claimed) or end c.Name's claim on
// it. c.Name must be set: a claim is made with the owner's own token.
func (c *Client) Claim(ctx context.Context, url, bearerToken string, claimed bool) error {
	cl, ctx, err := c.connect(ctx, url, bearerToken)
	if err != nil {
		return &ClaimError{State: adapter.Unreachable, Reason: err.Error()}
	}
	defer cl.Destroy()
	text := "release"
	if claimed {
		text = "claim"
	}
	am := &sdk.Message{
		ID: msg.NewUUIDv7(), Role: sdk.MessageRoleUser, Parts: sdk.ContentParts{sdk.NewTextPart(text)},
		Metadata: map[string]any{MetaKey: map[string]any{"claim": map[string]any{"owner": c.Name, "claimed": claimed}}},
	}
	_, err = cl.SendMessage(ctx, &sdk.SendMessageRequest{Message: am, Config: &sdk.SendMessageConfig{ReturnImmediately: true}})
	if err == nil {
		return nil
	}
	var ae *sdk.Error
	if errors.As(err, &ae) {
		if ct, ok := ae.Details[MetaKey].(map[string]any); ok {
			st, _ := ct["state"].(string)
			reason, _ := ct["reason"].(string)
			if st != "" {
				return &ClaimError{State: adapter.State(st), Reason: reason}
			}
		}
	}
	return &ClaimError{State: errorState(err), Reason: err.Error()}
}
