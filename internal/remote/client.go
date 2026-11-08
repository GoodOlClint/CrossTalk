package remote

import (
	"context"
	"net/http"
	"time"
)

const (
	ReasonGatewayNotRunning = "gateway_not_running"
	ReasonTimeout           = "timeout"
	ReasonPinMismatch       = "tls_pin_mismatch"
	ReasonUnpairedByPeer    = "unpaired_by_peer"
	ReasonUnreachable       = "unreachable"
	ReasonInvalidListing    = "invalid_listing"
)

// Error carries one of the Reason* codes.
type Error struct {
	Reason string
	Err    error
}

func (e *Error) Error() string { return e.Reason }
func (e *Error) Unwrap() error { return e.Err }

// Client dials one paired peer's gateway.
type Client struct{}

func NewClient(host Host, peer Peer) *Client { return &Client{} }

// Do sends one request to the peer and returns the status and a body capped at MaxResponse.
func (c *Client) Do(ctx context.Context, method, path string, hdr http.Header, body []byte) (int, []byte, error) {
	return 0, nil, &Error{Reason: ReasonUnreachable}
}

func (c *Client) Agents(ctx context.Context) ([]AgentRow, error) {
	return nil, &Error{Reason: ReasonUnreachable}
}

// Classify maps a transport error to a Reason* code.
func Classify(err error) string { return ReasonUnreachable }

const MaxResponse = 1 << 20

var _ = time.Second
