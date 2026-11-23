//go:build !darwin && !linux

package opencode

import (
	"context"
	"errors"
)

func osListenerPIDs(context.Context, int) ([]int, error) {
	return nil, errors.New("listener owner lookup is not supported on this platform")
}
