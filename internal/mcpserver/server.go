package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const Version = "0.1.0"

type Options struct {
	Backend Backend
	// Registry holds descriptors and locks; its Dir is also the journal root.
	Registry *registry.Registry
	// Harnesses identify callers, in trust order (ADR 0008).
	Harnesses []harness.Harness
	// PPID is the parent pid every request context carries; 0 means os.Getppid().
	PPID int
}

type Server struct {
	backend   Backend
	reg       *registry.Registry
	harnesses []harness.Harness
	ppid      int
	mcp       *mcp.Server

	mu     sync.Mutex
	agents map[string]*Agent
	closed bool
}

func DefaultOptions(b Backend, hs ...harness.Harness) (Options, error) {
	dir, err := registry.DefaultDir()
	if err != nil {
		return Options{}, err
	}
	return Options{Backend: b, Registry: registry.New(dir), Harnesses: hs}, nil
}

func New(opt Options) *Server {
	s := &Server{
		backend:   opt.Backend,
		reg:       opt.Registry,
		harnesses: opt.Harnesses,
		ppid:      opt.PPID,
		agents:    map[string]*Agent{},
	}
	if s.backend == nil {
		s.backend = Unwired{}
	}
	if s.ppid == 0 {
		s.ppid = os.Getppid()
	}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "crosstalk", Version: Version}, nil)
	s.addTools()
	return s
}

func (s *Server) MCP() *mcp.Server { return s.mcp }

// Run serves MCP on t until the client disconnects, then closes every agent.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	err := s.mcp.Run(ctx, t)
	return errors.Join(err, s.Close())
}

// Close removes this process's own-instance descriptors and releases their locks.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	agents := s.agents
	s.agents = map[string]*Agent{}
	s.mu.Unlock()
	var errs []error
	ac, _ := s.backend.(AgentCloser)
	for _, a := range agents {
		<-a.ready
		if a.err == nil {
			var cerr error
			if ac != nil {
				cerr = ac.CloseAgent(a)
				errs = append(errs, cerr)
			}
			if !errors.Is(cerr, ErrCloseTimeout) {
				errs = append(errs, a.close())
			}
		}
	}
	return errors.Join(errs...)
}

// agent returns the bound agent for c, registering it on the first bound
// call. A call that finds its agent's registration still running gets
// not_ready; it never waits on or binds to another caller's registration.
func (s *Server) agent(ctx context.Context, c caller) (*Agent, string, *Error) {
	id := identity.AgentID(string(c.harness), c.sessionRef)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, "", &Error{Code: CodeNotReady, Reason: "shutting_down"}
	}
	a, ok := s.agents[id]
	if !ok {
		a = &Agent{id: id, harness: c.harness, sessionRef: c.sessionRef, req: c.req, ready: make(chan struct{})}
		s.agents[id] = a
		s.mu.Unlock()
		notice, err := s.register(ctx, a, c)
		if err != nil {
			s.mu.Lock()
			delete(s.agents, id)
			s.mu.Unlock()
			a.err = err
			close(a.ready)
			return nil, "", toolError(err)
		}
		close(a.ready)
		return a, notice, nil
	}
	s.mu.Unlock()
	select {
	case <-a.ready:
	default:
		return nil, "", &Error{Code: CodeNotReady, Reason: "binding_in_progress"}
	}
	if a.err != nil {
		return nil, "", &Error{Code: CodeNotReady, Reason: "binding_failed"}
	}
	return a, s.followRename(a, c), nil
}

// register runs the §3 order: agent and alias locks, journal replay, Binder,
// then the atomic descriptor publish.
func (s *Server) register(ctx context.Context, a *Agent, c caller) (notice string, err error) {
	g, err := s.reg.Acquire(registry.Request{AgentID: a.id, HarnessName: c.name, Harness: string(c.harness), Cwd: c.cwd})
	if errors.Is(err, registry.ErrAgentHeld) {
		return "", rejected("agent_held")
	}
	if err != nil {
		return "", err
	}
	a.reg = g
	defer func() {
		if err != nil {
			if ac, ok := s.backend.(AgentCloser); ok {
				if errors.Is(ac.CloseAgent(a), ErrCloseTimeout) {
					return
				}
			}
			a.close()
		}
	}()
	var endpoint string
	if hb, ok := s.backend.(HarnessBinder); ok {
		b, err := hb.BindHarness(ctx, a)
		if err != nil {
			return "", err
		}
		endpoint = b.HarnessEndpoint
	}
	j, rp, err := journal.Open(s.reg.Dir, a.id, journal.Binding{InstanceID: g.InstanceID(), HarnessEndpoint: endpoint, SessionRef: c.sessionRef}, journal.Options{})
	if err != nil {
		return "", err
	}
	a.j, a.replay = j, rp
	a.harnessName = c.name

	d := registry.Descriptor{Harness: string(c.harness), Cwd: c.cwd, SessionRef: c.sessionRef, Role: registry.RolePrimary}
	if c.harnessPID > 0 {
		pid := c.harnessPID
		d.HarnessPID = &pid
		if st, err := s.reg.StartTime(pid); err == nil {
			d.HarnessStartedAt = &st
		}
	}
	if r, ok := j.Latest(journal.Record{Type: journal.TypeClaim}); ok && r.Claimed {
		owner := s.nameOf(r.Owner)
		d.Role, d.Owner = registry.RoleSubagent, &owner
	}
	if b, ok := s.backend.(Binder); ok {
		info, err := b.Bind(ctx, a)
		if err != nil {
			return "", err
		}
		d.A2AURL, d.CredentialRef = info.A2AURL, info.CredentialRef
		if info.HarnessEndpoint != "" {
			d.HarnessEndpoint = &info.HarnessEndpoint
		}
		if info.HarnessVersion != "" {
			d.HarnessVersion = &info.HarnessVersion
		}
	}
	if err := g.Publish(d); err != nil {
		return "", err
	}
	if p, ok := s.backend.(Publisher); ok {
		p.Published(a)
	}
	if c.name != "" && g.Name() != c.name {
		notice = fmt.Sprintf("registered as %q: harness name %q is held by another live agent", g.Name(), c.name)
	}
	return notice, nil
}

// followRename moves the alias when the harness session name changed since
// the last call; agent_id and every agent_id-keyed path stay (CQ9).
func (s *Server) followRename(a *Agent, c caller) string {
	if c.name == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c.name == a.harnessName {
		return ""
	}
	err := a.reg.Rename(c.name)
	switch {
	case err == nil:
		a.harnessName = c.name
		return ""
	case errors.Is(err, registry.ErrNameHeld):
		a.harnessName = c.name
		return fmt.Sprintf("still registered as %q: harness name %q is held by another live agent", a.reg.Name(), c.name)
	default:
		return fmt.Sprintf("rename to %q failed: %v", c.name, err)
	}
}

// nameOf returns the live alias of agentID, or agentID itself.
func (s *Server) nameOf(agentID string) string {
	ds, err := s.reg.List()
	if err == nil {
		for _, d := range ds {
			if d.AgentID == agentID {
				return d.Name
			}
		}
	}
	return agentID
}

// local returns the ready agent agentID served by this process, if any.
func (s *Server) local(agentID string) *Agent {
	s.mu.Lock()
	a := s.agents[agentID]
	s.mu.Unlock()
	if a == nil || !a.isReady() {
		return nil
	}
	return a
}

// resolve takes name and cwd from Discover when the harness could not name
// the caller from the request alone.
func (s *Server) resolve(ctx context.Context, req *mcp.CallToolRequest) (caller, error) {
	c, err := resolveCaller(ctx, s.harnesses, requestContext(req, s.ppid))
	if err != nil || !c.bound() || c.name != "" {
		return c, err
	}
	sessions, err := s.backend.Discover(ctx)
	if err != nil {
		return c, nil
	}
	for _, ss := range sessions {
		if ss.Harness == c.harness && ss.SessionRef == c.sessionRef {
			c.name, c.cwd = ss.Name, ss.Cwd
			break
		}
	}
	return c, nil
}

func toolError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeFailed, Reason: err.Error()}
}

func errResult(e *Error) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{IsError: true}, e, nil
}

func withNotice(out any, notice string) any {
	if notice == "" {
		return out
	}
	b, err := json.Marshal(out)
	if err != nil {
		return out
	}
	m := map[string]any{}
	if json.Unmarshal(b, &m) != nil {
		return out
	}
	m["notice"] = notice
	return m
}

// boundTool registers a tool served only to bound callers (§8).
func boundTool[In any](s *Server, t *mcp.Tool, h func(context.Context, *Agent, In) (any, error)) {
	mcp.AddTool(s.mcp, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		c, err := s.resolve(ctx, req)
		if err != nil {
			return errResult(toolError(err))
		}
		if !c.bound() {
			return errResult(&Error{Code: CodeUnbound, Reason: "no harness claims this call"})
		}
		a, notice, e := s.agent(ctx, c)
		if e != nil {
			return errResult(e)
		}
		out, err := h(ctx, a, in)
		if err != nil {
			return errResult(toolError(err))
		}
		return nil, withNotice(out, notice), nil
	})
}

type listIn struct{}

type sendIn struct {
	To       string `json:"to" jsonschema:"agent name or agent_id"`
	Text     string `json:"text"`
	Kind     string `json:"kind,omitempty" jsonschema:"routing hint; not a trust boundary"`
	Delivery string `json:"delivery,omitempty" jsonschema:"Codex scheduling; no-op for Claude"`
}

type replyIn struct {
	MessageID string `json:"message_id" jsonschema:"the inbound message being answered"`
	Text      string `json:"text"`
}

type askIn struct {
	To              string `json:"to" jsonschema:"agent name or agent_id"`
	Text            string `json:"text"`
	DeadlineSeconds int    `json:"deadline_seconds,omitempty" jsonschema:"0 = no deadline"`
}

type receiveIn struct {
	After string `json:"after,omitempty" jsonschema:"message_id to paginate after; set to page the full journal"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum entries; 0 = backend default"`
}

type nameIn struct {
	Name string `json:"name" jsonschema:"agent name or agent_id"`
}

func schemaFor[T any](edit func(*jsonschema.Schema)) *jsonschema.Schema {
	sch, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	if edit != nil {
		edit(sch)
	}
	return sch
}

func enumProp(sch *jsonschema.Schema, prop, def string, vals ...string) {
	p := sch.Properties[prop]
	for _, v := range vals {
		p.Enum = append(p.Enum, v)
	}
	p.Default = json.RawMessage(`"` + def + `"`)
}

func (s *Server) addTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_agents",
		Description: "List every discovered agent session, with or without CrossTalk.",
		InputSchema: schemaFor[listIn](nil),
	}, s.listAgentsTool)

	boundTool(s, &mcp.Tool{
		Name:        "send_message",
		Description: "Send text to an agent by name or agent_id. Returns {message_id, state}.",
		InputSchema: schemaFor[sendIn](func(sch *jsonschema.Schema) {
			enumProp(sch, "kind", "inform", "inform", "command", "ask")
			enumProp(sch, "delivery", "immediate", "immediate", "next_turn")
		}),
	}, func(ctx context.Context, a *Agent, in sendIn) (any, error) {
		k, d := msg.Kind(in.Kind), msg.Delivery(in.Delivery)
		if k == "" {
			k = msg.Inform
		}
		if d == "" {
			d = msg.Immediate
		}
		if !k.Valid() {
			return nil, rejected("unknown_kind")
		}
		if !d.Valid() {
			return nil, &Error{Code: CodeInvalid, Reason: "delivery"}
		}
		return s.backend.Send(ctx, a, SendRequest{To: in.To, Text: in.Text, Kind: k, Delivery: d})
	})

	boundTool(s, &mcp.Tool{
		Name:        "reply",
		Description: "Answer an inbound message; the destination comes from the stored original. Returns {message_id, state}.",
		InputSchema: schemaFor[replyIn](nil),
	}, func(ctx context.Context, a *Agent, in replyIn) (any, error) {
		return s.backend.Reply(ctx, a, in.MessageID, in.Text)
	})

	boundTool(s, &mcp.Tool{
		Name:        "ask",
		Description: "Send a reply-required message to an agent by name or agent_id. Returns {message_id, state} at once; the answer arrives through reply.",
		InputSchema: schemaFor[askIn](func(sch *jsonschema.Schema) {
			zero := 0.0
			sch.Properties["deadline_seconds"].Minimum = &zero
		}),
	}, func(ctx context.Context, a *Agent, in askIn) (any, error) {
		if in.DeadlineSeconds < 0 {
			return nil, &Error{Code: CodeInvalid, Reason: "deadline_seconds"}
		}
		return s.backend.Ask(ctx, a, AskRequest{To: in.To, Text: in.Text, DeadlineSeconds: in.DeadlineSeconds})
	})

	boundTool(s, &mcp.Tool{
		Name:        "receive_message",
		Description: "Pull mailbox entries the harness did not deliver (inbound unreachable, unknown, recovered, bounced) and your own sends still unknown, e.g. pending far-side tasks (direction outbound). after is a message_id cursor over the full journal ordering, but the state filter always applies: accepted and queued messages are never returned.",
		InputSchema: schemaFor[receiveIn](func(sch *jsonschema.Schema) {
			zero := 0.0
			sch.Properties["limit"].Minimum = &zero
		}),
	}, func(ctx context.Context, a *Agent, in receiveIn) (any, error) {
		es, err := s.backend.Mailbox(ctx, a, in.After, in.Limit)
		if err != nil {
			return nil, err
		}
		if es == nil {
			es = []MailboxEntry{}
		}
		return map[string]any{"messages": es}, nil
	})

	s.addClaimTools()
}
