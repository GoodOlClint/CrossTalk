package peercred

import "golang.org/x/sys/unix"

// xucred carries uid/gids only; the pid needs LOCAL_PEERPID at level SOL_LOCAL.
func peerPID(fd int) (int, error) {
	return unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}
