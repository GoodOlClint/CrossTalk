package opencode

import (
	"context"
	"net/url"
	"strconv"
)

// listenerPIDs returns the pid of every process holding a TCP LISTEN socket on
// port; none means no listener this user can see.
var listenerPIDs = osListenerPIDs

// ownerRefusal is empty when every listener on the origin's port belongs to
// pid, and otherwise says why the request may not leave.
func ownerRefusal(ctx context.Context, origin string, pid int) string {
	u, err := url.Parse(origin)
	if err != nil {
		return "service port unreadable; nothing sent"
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return "service port unreadable; nothing sent"
	}
	pids, err := listenerPIDs(ctx, port)
	if err != nil || len(pids) == 0 {
		return "owner of the service port could not be determined; nothing sent"
	}
	for _, p := range pids {
		if p != pid {
			return "service port is held by a process other than the service pid; nothing sent"
		}
	}
	return ""
}
