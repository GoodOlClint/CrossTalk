package peercred

import (
	"errors"
	"net"
)

var ErrUnsupported = errors.New("peercred: unsupported platform")

// PeerPID returns the pid of the process on the other end of conn, as recorded
// by the kernel when the connection was established.
func PeerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var serr error
	if err := raw.Control(func(fd uintptr) { pid, serr = peerPID(int(fd)) }); err != nil {
		return 0, err
	}
	return pid, serr
}
