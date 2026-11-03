//go:build !darwin && !linux

package peercred

func peerPID(int) (int, error) { return 0, ErrUnsupported }
