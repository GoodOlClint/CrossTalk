package remote

import (
	"context"
	"errors"
	"time"
)

var errNotImplemented = errors.New("remote: pairing not implemented")

// Pairing is one pending.json entry (plan sections 6 and 7).
type Pairing struct {
	ID        string    `json:"id"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
	Alias     string    `json:"alias,omitempty"`
}

// PairToken is the one-line token `remote pair` prints.
type PairToken struct {
	Address     string
	Port        int
	Fingerprint string
	ID          string
	Secret      string
}

func (t PairToken) String() string { return "" }

func ParsePairToken(s string) (PairToken, error) { return PairToken{}, errNotImplemented }

func StartPairing(stateDir string, now time.Time, ttl time.Duration, alias string) (Pairing, error) {
	return Pairing{}, errNotImplemented
}

func CancelPairing(stateDir, id string, now time.Time) error { return errNotImplemented }

type JoinOptions struct {
	Host    Host
	Config  Config
	Peers   *Peers
	Token   PairToken
	Alias   string
	Timeout time.Duration
}

func JoinGateway(ctx context.Context, o JoinOptions) (Peer, error) { return Peer{}, errNotImplemented }

type WaitOptions struct {
	StateDir string
	Peers    *Peers
	ID       string
	Since    time.Time
	Poll     time.Duration
	Now      func() time.Time
}

func WaitPaired(ctx context.Context, o WaitOptions) (Peer, error) { return Peer{}, errNotImplemented }

func GatewayRunning(stateDir string) (bool, error) { return false, errNotImplemented }

type pairRequest struct {
	ID      string `json:"id"`
	Alias   string `json:"alias"`
	Address string `json:"address"`
	MAC     string `json:"mac"`
}

var (
	errPairUnknown = errors.New("remote: unknown pairing id")
	errPairExpired = errors.New("remote: pairing expired")
	errPairProof   = errors.New("remote: bad pairing proof")
	errPairAlias   = errors.New("remote: alias belongs to another peer")
)

func redeemPairing(stateDir string, peers *Peers, now time.Time, req pairRequest, clientFP, serverFP string) error {
	return errNotImplemented
}
