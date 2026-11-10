//go:build !darwin && !linux

package ownerproof

import "errors"

func nonDumpable() error { return errors.ErrUnsupported }
