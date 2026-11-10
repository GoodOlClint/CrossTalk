package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type renameOut struct {
	Name     string `json:"name"`
	Previous string `json:"previous"`
}

func (s *Server) addRenameTool() {
	boundTool(s, &mcp.Tool{
		Name:        "rename",
		Description: "Set this session's own name, which list_agents shows and to resolves. The harness's name for the session no longer overrides it. Returns {name, previous}.",
		InputSchema: schemaFor[nameIn](nil),
	}, func(ctx context.Context, a *Agent, in nameIn) (any, error) {
		return s.rename(a, in.Name)
	})
}

// rename takes the new alias lock before journaling the pin, so a crash
// between the two leaves a live name without a pin.
func (s *Server) rename(a *Agent, name string) (any, error) {
	if identity.ValidName(name) != nil {
		return nil, rejected("invalid_name")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	previous := a.reg.Name()
	if err := a.reg.Rename(name); err != nil {
		if errors.Is(err, registry.ErrNameHeld) {
			return nil, rejected("name_held")
		}
		return nil, err
	}
	if err := a.j.Append(journal.Record{Type: journal.TypeName, Name: name}); err != nil {
		if rerr := a.reg.Rename(previous); rerr != nil {
			return nil, &Error{Code: CodeFailed, Reason: err.Error(), Message: fmt.Sprintf("the name is now %q and not saved: %v", a.reg.Name(), rerr)}
		}
		return nil, err
	}
	a.pinned = name
	return renameOut{Name: name, Previous: previous}, nil
}
