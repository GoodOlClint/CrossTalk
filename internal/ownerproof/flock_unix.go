//go:build darwin || linux

package ownerproof

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockHeld(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return false, unix.Flock(int(f.Fd()), unix.LOCK_UN)
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	return false, err
}
