package remote

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

const StatePortNotBound = "service running, port not bound"

var ErrPortHeld = errors.New("remote: port is held by another process")

type Limits struct {
	HandshakeTimeout, HeaderTimeout, ReadTimeout, WriteTimeout, IdleTimeout time.Duration
	MaxBody                                                                 int64
	MaxPinnedConns, MaxPeerConcurrent, MaxPeerPerMinute                     int
	MaxHandshaking, MaxUnenrolled                                           int
	UnenrolledLife, PendingPoll, BindRetry, BindGiveUp                      time.Duration
}

func DefaultLimits() Limits { return Limits{} }

type GatewayConfig struct {
	StateDir string
	Host     Host
	Bind     string
	Port     int
	Peers    *Peers
	Handler  http.Handler
	Limits   Limits
	Now      func() time.Time
	Listen   func(network, addr string) (net.Listener, error)
	Log      func(format string, args ...any)
}

type Gateway struct{ cfg GatewayConfig }

func NewGateway(c GatewayConfig) *Gateway { return &Gateway{cfg: c} }

func (g *Gateway) Serve(ctx context.Context) error { return errors.New("not implemented") }
func (g *Gateway) Ready() <-chan struct{}          { return make(chan struct{}) }
func (g *Gateway) Addr() net.Addr                  { return nil }
func (g *Gateway) State() string                   { return "" }

func EnsureToken(stateDir string) (string, error) { return "", errors.New("not implemented") }
