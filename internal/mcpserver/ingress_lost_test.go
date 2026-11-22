package mcpserver

import (
	"testing"
	"time"
)

type lostBackend struct {
	*FakeBackend
	lost []*Agent
}

func (b *lostBackend) PauseIngress() func() []*Agent { return func() []*Agent { return b.lost } }

func readyAgent(id string, ready bool) *Agent {
	a := &Agent{id: id, ready: make(chan struct{})}
	if ready {
		close(a.ready)
	}
	return a
}

func (s *Server) current(id string) *Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agents[id]
}

// A lost listener names the instance that lost it: a replacement registered
// under the same agent_id meanwhile is kept.
func TestIngressLostKeepsReplacementAgent(t *testing.T) {
	old, repl := readyAgent("x", true), readyAgent("x", true)
	s := &Server{backend: &lostBackend{NewFakeBackend(), []*Agent{old}}, agents: map[string]*Agent{"x": repl}}
	s.PauseIngress()()
	if s.current("x") != repl {
		t.Fatal("the replacement agent was retired for its predecessor's lost listener")
	}
}

// An agent still registering when its listener is lost is retired once its
// registration succeeds.
func TestIngressLostRetiresAgentAfterRegistration(t *testing.T) {
	a := readyAgent("x", false)
	s := &Server{backend: &lostBackend{NewFakeBackend(), []*Agent{a}}, agents: map[string]*Agent{"x": a}}
	s.PauseIngress()()
	close(a.ready)
	deadline := time.Now().Add(5 * time.Second)
	for s.current("x") != nil {
		if time.Now().After(deadline) {
			t.Fatal("an agent that lost its listener while registering was not retired")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
