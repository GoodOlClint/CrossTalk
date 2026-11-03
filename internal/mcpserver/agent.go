package mcpserver

import (
	"sync"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// Agent is one bound session's state inside this process. A pooled Codex
// connection holds several.
type Agent struct {
	id         string
	harness    adapter.Harness
	sessionRef string

	ready chan struct{}
	err   error

	// mu serializes alias changes and claim/release applied to this agent.
	mu          sync.Mutex
	harnessName string
	reg         *registry.Registration
	j           *journal.Journal
	replay      journal.Replay
}

func (a *Agent) ID() string                           { return a.id }
func (a *Agent) Harness() adapter.Harness             { return a.harness }
func (a *Agent) SessionRef() string                   { return a.sessionRef }
func (a *Agent) Name() string                         { return a.reg.Name() }
func (a *Agent) Descriptor() registry.Descriptor      { return a.reg.Descriptor() }
func (a *Agent) Journal() *journal.Journal            { return a.j }
func (a *Agent) Registration() *registry.Registration { return a.reg }

// Replay is what journal.Open returned for this registration; a Binder
// passes it to receiver.Recover.
func (a *Agent) Replay() journal.Replay { return a.replay }

func (a *Agent) isReady() bool {
	select {
	case <-a.ready:
		return a.err == nil
	default:
		return false
	}
}

func (a *Agent) close() error {
	var errs []error
	if a.j != nil {
		errs = append(errs, a.j.Close())
	}
	if a.reg != nil {
		errs = append(errs, a.reg.Close())
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
