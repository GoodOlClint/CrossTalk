//go:build !darwin && !linux

package procstart

import (
	"errors"
	"time"
)

func StartTime(int) (time.Time, error) {
	return time.Time{}, errors.New("procstart: unsupported platform")
}
