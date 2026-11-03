package adapter

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GoodOlClint/crosstalk/internal/msg"
)

var ErrDisconnected = errors.New("fake: disconnected")

type Outcome struct {
	State State
	Err   error
	// NoWrite marks an outcome that fails before anything reaches the harness.
	NoWrite bool
	// Entered, when set, receives one value when Inject starts, before Gate.
	Entered chan<- struct{}
	// Gate, when set, holds Inject before the harness write until it is
	// closed or ctx is done; a ctx-ended wait records no write.
	Gate <-chan struct{}
	// After runs once the write is recorded, e.g. to flip Status mid-call.
	After func(Call)
}

type Op string

const (
	OpRegister Op = "register"
	OpInject   Op = "inject"
	OpStatus   Op = "status"
)

type Call struct {
	Op         Op
	SessionRef string
	Message    msg.Message
	Delivery   Delivery
	Wrote      bool
	State      State
	Err        error
}

type Fake struct {
	Harness Harness

	mu       sync.Mutex
	injects  []Outcome
	defaultO Outcome
	statuses map[string][]SessionStatus
	bindings map[string]Binding
	regErr   map[string]error
	calls    []Call
}

var _ Adapter = (*Fake)(nil)

func NewFake(h Harness) *Fake {
	return &Fake{
		Harness:  h,
		defaultO: Outcome{State: Accepted},
		statuses: map[string][]SessionStatus{},
		bindings: map[string]Binding{},
		regErr:   map[string]error{},
	}
}

// ScriptInject queues outcomes consumed one per Inject call, in order.
func (f *Fake) ScriptInject(o ...Outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injects = append(f.injects, o...)
}

func (f *Fake) SetDefaultInject(o Outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultO = o
}

// ScriptStatus queues statuses for sessionRef; the last one repeats.
func (f *Fake) ScriptStatus(sessionRef string, s ...SessionStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[sessionRef] = append(f.statuses[sessionRef], s...)
}

func (f *Fake) SetBinding(b Binding) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings[b.SessionRef] = b
}

func (f *Fake) SetRegisterError(sessionRef string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regErr[sessionRef] = err
}

func (f *Fake) Register(_ context.Context, sessionRef string) (Binding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.regErr[sessionRef]
	b, ok := f.bindings[sessionRef]
	if !ok {
		b = Binding{Harness: f.Harness, SessionRef: sessionRef, HarnessEndpoint: "fake://" + sessionRef}
	}
	f.calls = append(f.calls, Call{Op: OpRegister, SessionRef: sessionRef, Err: err})
	if err != nil {
		return Binding{}, err
	}
	return b, nil
}

func (f *Fake) Status(_ context.Context, sessionRef string) (SessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.statuses[sessionRef]
	var s SessionStatus
	switch len(q) {
	case 0:
		s = SessionStatus{Loaded: true, Reachable: true}
	case 1:
		s = q[0]
	default:
		s, f.statuses[sessionRef] = q[0], q[1:]
	}
	f.calls = append(f.calls, Call{Op: OpStatus, SessionRef: sessionRef})
	return s, nil
}

func (f *Fake) Inject(ctx context.Context, m msg.Message, d Delivery) (State, error) {
	f.mu.Lock()
	o := f.defaultO
	if len(f.injects) > 0 {
		o, f.injects = f.injects[0], f.injects[1:]
	}
	f.mu.Unlock()

	if !o.State.Injectable() {
		panic(fmt.Sprintf("adapter.Fake: scripted state %q is not an inject result", o.State))
	}
	if o.Entered != nil {
		o.Entered <- struct{}{}
	}
	call := Call{Op: OpInject, SessionRef: d.Target.SessionRef, Message: m, Delivery: d}
	if o.Gate != nil {
		select {
		case <-o.Gate:
		case <-ctx.Done():
			call.State, call.Err = Unknown, ctx.Err()
			f.record(call)
			return call.State, call.Err
		}
	}
	call.Wrote = !o.NoWrite
	call.State, call.Err = o.State, o.Err
	f.record(call)
	if o.After != nil {
		o.After(call)
	}
	return o.State, o.Err
}

func (f *Fake) record(c Call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Writes returns the Inject calls whose payload reached the fake harness.
func (f *Fake) Writes() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var w []Call
	for _, c := range f.calls {
		if c.Op == OpInject && c.Wrote {
			w = append(w, c)
		}
	}
	return w
}
