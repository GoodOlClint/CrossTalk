package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MaxFrame bounds one inbound frame, the same cap as the SDK's stdio transport.
const MaxFrame = mcp.DefaultMaxLineLength

// batchProtocol is the first protocol version that forbids JSON-RPC batches.
const batchProtocol = "2025-06-18"

// defaultProtocol is what the SDK assumes before a version is negotiated.
const defaultProtocol = "2025-03-26"

var errFrameTooLarge = errors.New("inbound JSON-RPC frame exceeded the maximum line length")

// SessionState is what the Conn saw on the wire: the client's initialize
// params, the protocol version in the server's initialize response, whether
// notifications/initialized arrived, and the last logging/setLevel params.
type SessionState struct {
	InitializeParams json.RawMessage
	ProtocolVersion  string
	Initialized      bool
	SetLevelParams   json.RawMessage
}

// Transport is the shim's own mcp.Transport over a byte stream pair. Unlike
// the SDK's stdio transport it never reads ahead of the frame it returns, and
// it knows which requests still owe a response.
type Transport struct {
	conn *Conn
	used atomic.Bool
}

// NewTransport builds the transport and its connection up front, so a watcher
// can hold Conn before Server.Run starts.
func NewTransport(in io.Reader, out io.Writer) *Transport {
	return &Transport{conn: NewConn(in, out)}
}

// Conn returns the connection Connect hands out.
func (t *Transport) Conn() *Conn { return t.conn }

// Connect implements mcp.Transport; it serves one connection.
func (t *Transport) Connect(context.Context) (mcp.Connection, error) {
	if !t.used.CompareAndSwap(false, true) {
		return nil, errors.New("transport already connected")
	}
	return t.conn, nil
}

// Conn is a newline-delimited JSON-RPC connection. Frame bytes are read one at
// a time up to '\n' so nothing beyond the returned frame is consumed from In.
type Conn struct {
	in  io.Reader
	out io.Writer

	wmu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
	closer    io.Closer

	smu      sync.Mutex
	pending  map[jsonrpc.ID]struct{}
	cand     map[jsonrpc.ID]candidate // state-changing requests until their response succeeds
	werr     error                    // a failed response write; handoff must not proceed
	batches  map[jsonrpc.ID]*batch
	queue    []jsonrpc.Message
	inflight int // responses being written
	state    SessionState
	hasInit  bool
	partial  bool
	rd       chan frameRes // the outstanding frame read, if a Read gave up on it
	idle     chan struct{} // closed and replaced on every change
}

type candidate struct {
	method string
	params json.RawMessage
}

type batch struct {
	unresolved map[jsonrpc.ID]int
	responses  []*jsonrpc.Response
}

// NewConn builds a Conn over in and out. Close closes in when it is an io.Closer.
func NewConn(in io.Reader, out io.Writer) *Conn {
	c := &Conn{
		in:      in,
		out:     out,
		closed:  make(chan struct{}),
		pending: map[jsonrpc.ID]struct{}{},
		cand:    map[jsonrpc.ID]candidate{},
		batches: map[jsonrpc.ID]*batch{},
		idle:    make(chan struct{}),
	}
	if cl, ok := in.(io.Closer); ok {
		c.closer = cl
	}
	return c
}

func (c *Conn) SessionID() string { return "" }

func (c *Conn) changed() {
	close(c.idle)
	c.idle = make(chan struct{})
}

// Read returns the next message. It consumes exactly one frame from the input.
func (c *Conn) Read(ctx context.Context) (jsonrpc.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.smu.Lock()
	if len(c.queue) > 0 {
		m := c.queue[0]
		c.queue = c.queue[1:]
		c.changed()
		c.smu.Unlock()
		return m, nil
	}
	c.smu.Unlock()

	c.smu.Lock()
	ch := c.rd
	if ch == nil {
		ch = make(chan frameRes, 1)
		c.rd = ch
		go func() {
			line, err := c.readFrame()
			ch <- frameRes{line, err}
		}()
	}
	c.smu.Unlock()
	var r frameRes
	select {
	case r = <-ch:
		c.smu.Lock()
		c.rd = nil
		c.smu.Unlock()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, io.EOF
	}
	if r.err != nil {
		return nil, r.err
	}
	return c.accept(r.line)
}

type frameRes struct {
	line []byte
	err  error
}

// readFrame runs only while a Read waits for it, so it never reads ahead of a
// frame nobody asked for. It reads one byte at a time up to '\n', so nothing past the frame is
// consumed. Close closes the input, which unblocks a Read in progress.
func (c *Conn) readFrame() ([]byte, error) {
	var line []byte
	var one [1]byte
	for {
		n, err := c.in.Read(one[:])
		if n == 0 && err != nil {
			select {
			case <-c.closed:
				err = io.EOF
			default:
			}
			if len(line) > 0 && errors.Is(err, io.EOF) {
				c.setPartial(false)
				return line, nil
			}
			c.setPartial(false)
			return nil, err
		}
		if n == 0 {
			continue
		}
		if one[0] == '\n' {
			if len(line) == 0 {
				continue
			}
			return line, nil
		}
		if len(line) == 0 {
			c.setPartial(true)
		}
		line = append(line, one[0])
		if len(line) > MaxFrame {
			c.setPartial(false)
			return nil, errFrameTooLarge
		}
	}
}

func (c *Conn) setPartial(p bool) {
	c.smu.Lock()
	if c.partial != p {
		c.partial = p
		c.changed()
	}
	c.smu.Unlock()
}

func (c *Conn) accept(line []byte) (jsonrpc.Message, error) {
	var arr []json.RawMessage
	isBatch := json.Unmarshal(line, &arr) == nil
	var msgs []jsonrpc.Message
	if isBatch {
		if len(arr) == 0 {
			return nil, fmt.Errorf("empty batch")
		}
		for _, raw := range arr {
			m, err := jsonrpc.DecodeMessage(raw)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, m)
		}
	} else {
		m, err := jsonrpc.DecodeMessage(line)
		if err != nil {
			return nil, err
		}
		msgs = []jsonrpc.Message{m}
	}

	c.smu.Lock()
	defer c.smu.Unlock()
	if c.partial {
		c.partial = false
		c.changed()
	}
	if isBatch {
		v := c.state.ProtocolVersion
		if v == "" {
			v = defaultProtocol
		}
		if v >= batchProtocol {
			return nil, fmt.Errorf("JSON-RPC batching is not supported in %s and later (request version: %s)", batchProtocol, v)
		}
		var b *batch
		for _, m := range msgs {
			if req, ok := m.(*jsonrpc.Request); ok && req.ID.IsValid() {
				if b == nil {
					b = &batch{unresolved: map[jsonrpc.ID]int{}}
				}
				if _, dup := b.unresolved[req.ID]; dup {
					return nil, fmt.Errorf("duplicate message ID %q", req.ID)
				}
				b.unresolved[req.ID] = len(b.responses)
				b.responses = append(b.responses, nil)
			}
		}
		if b != nil {
			for id := range b.unresolved {
				if _, ok := c.batches[id]; ok {
					return nil, fmt.Errorf("batch contains previously seen request %v", id.Raw())
				}
			}
			for id := range b.unresolved {
				c.batches[id] = b
			}
		}
	}
	for _, m := range msgs {
		c.observeRead(m)
	}
	c.queue = msgs[1:]
	c.changed()
	return msgs[0], nil
}

// observeRead records pending ids and captures client state; smu is held.
func (c *Conn) observeRead(m jsonrpc.Message) {
	req, ok := m.(*jsonrpc.Request)
	if !ok {
		return
	}
	if req.ID.IsValid() {
		c.pending[req.ID] = struct{}{}
	}
	switch req.Method {
	case "initialize":
		if !c.hasInit {
			c.cand[req.ID] = candidate{method: req.Method, params: append(json.RawMessage(nil), req.Params...)}
		}
	case "notifications/initialized":
		c.state.Initialized = true
	case "logging/setLevel":
		c.cand[req.ID] = candidate{method: req.Method, params: append(json.RawMessage(nil), req.Params...)}
	}
}

// Write writes msg; a response leaves the pending set only after its bytes are
// written, and a failed write is terminal for quiescence.
func (c *Conn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resp, isResp := msg.(*jsonrpc.Response)

	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.smu.Lock()
	c.inflight++
	c.smu.Unlock()
	var err error
	defer func() {
		c.smu.Lock()
		c.inflight--
		if err != nil {
			c.werr = err
		} else if isResp {
			delete(c.pending, resp.ID)
		}
		c.changed()
		c.smu.Unlock()
	}()
	if isResp {
		c.commit(resp)
		if b, ok := c.takeBatch(resp); ok {
			if len(b) == 0 {
				return nil
			}
			err = c.writeBatch(b)
			return err
		}
	}
	var data []byte
	data, err = jsonrpc.EncodeMessage(msg)
	if err != nil {
		err = fmt.Errorf("marshaling message: %v", err)
		return err
	}
	_, err = c.out.Write(append(data, '\n'))
	return err
}

func (c *Conn) takeBatch(resp *jsonrpc.Response) ([]*jsonrpc.Response, bool) {
	c.smu.Lock()
	defer c.smu.Unlock()
	b, ok := c.batches[resp.ID]
	if !ok {
		return nil, false
	}
	b.responses[b.unresolved[resp.ID]] = resp
	delete(b.unresolved, resp.ID)
	delete(c.batches, resp.ID)
	if len(b.unresolved) == 0 {
		return b.responses, true
	}
	return nil, true
}

func (c *Conn) writeBatch(rs []*jsonrpc.Response) error {
	raws := make([]json.RawMessage, 0, len(rs))
	for _, r := range rs {
		raw, err := jsonrpc.EncodeMessage(r)
		if err != nil {
			return fmt.Errorf("encoding batch message: %w", err)
		}
		raws = append(raws, raw)
	}
	data, err := json.Marshal(raws)
	if err != nil {
		return err
	}
	_, err = c.out.Write(append(data, '\n'))
	return err
}

// commit applies a state-changing request's candidate once its response is a
// success; it runs before the response bytes are written, so a client's next
// frame already sees the negotiated version.
func (c *Conn) commit(resp *jsonrpc.Response) {
	c.smu.Lock()
	defer c.smu.Unlock()
	cd, ok := c.cand[resp.ID]
	if !ok {
		return
	}
	delete(c.cand, resp.ID)
	if resp.Error != nil {
		return
	}
	switch cd.method {
	case "initialize":
		var r struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(resp.Result, &r) != nil {
			return
		}
		c.state.InitializeParams = cd.params
		c.state.ProtocolVersion = r.ProtocolVersion
		c.hasInit = true
	case "logging/setLevel":
		c.state.SetLevelParams = cd.params
	}
}

// Close implements mcp.Connection; it unblocks a pending Read.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		if c.closer != nil {
			c.closeErr = c.closer.Close()
		}
		close(c.closed)
	})
	return c.closeErr
}

// State returns a copy of what the Conn captured from the wire.
func (c *Conn) State() SessionState {
	c.smu.Lock()
	defer c.smu.Unlock()
	s := c.state
	s.InitializeParams = append(json.RawMessage(nil), s.InitializeParams...)
	s.SetLevelParams = append(json.RawMessage(nil), s.SetLevelParams...)
	return s
}

// Quiescent reports that no frame is half-read, no read request still owes a
// response, and no response is mid-write.
func (c *Conn) Quiescent() bool {
	c.smu.Lock()
	defer c.smu.Unlock()
	return !c.partial && len(c.pending) == 0 && c.inflight == 0 && len(c.queue) == 0 && c.werr == nil
}

// WaitQuiescent blocks until Quiescent or ctx ends.
func (c *Conn) WaitQuiescent(ctx context.Context) error {
	for {
		c.smu.Lock()
		q := !c.partial && len(c.pending) == 0 && c.inflight == 0 && len(c.queue) == 0 && c.werr == nil
		ch := c.idle
		c.smu.Unlock()
		if q {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
