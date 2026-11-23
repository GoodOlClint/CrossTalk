//go:build !darwin && !linux

package opencode

import (
	"context"
	"errors"
	"net/netip"
)

func osPeerPIDs(context.Context, netip.AddrPort, netip.AddrPort) ([]int, error) {
	return nil, errors.New("connection holder lookup is not supported on this platform")
}
