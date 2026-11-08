package shim

import (
	"context"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

func (s *Shim) ListRemote(ctx context.Context) mcpserver.RemoteListing {
	return mcpserver.RemoteListing{}
}

// RemoteSource is the gateway's view of this host's sessions.
func RemoteSource(srv *mcpserver.Server) remote.AgentsSource {
	return func(context.Context) []remote.AgentRow { return nil }
}
