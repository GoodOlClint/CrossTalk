package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

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
	In  io.Reader
	Out io.Writer
	// Conn is set by Connect.
	conn *Conn
}

// Conn returns the connection made by Connect, or nil before it.
func (t *Transport) Conn() *Conn { return t.conn }

// Connect implements mcp.Transport.
func (t *Transport) Connect(context.Context) (mcp.Connection, error) {
	t.conn = NewConn(t.In, t.Out)
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
	batches  map[jsonrpc.ID]*batch
	queue    []jsonrpc.Message
	inflight int // responses being written
	state    SessionState
	initID   jsonrpc.ID
	hasInit  bool
	partial  bool
	idle     chan struct{} // closed and replaced on every change
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
		c.smu.Unlock()
		return m, nil
	}
	c.smu.Unlock()

	line, err := c.readFrame()
	if err != nil {
		return nil, err
	}
	return c.accept(line)
}

// readFrame reads one byte at a time up to '\n', so nothing past the frame is
// consumed. Close closes the input, which unblocks a Read in progress.
func (c *Conn) readFrame() ([]byte, error) {
	var line []byte
	var one [1]byte
	for {
		n, err := c.in.Read(one[:])
		if n == 0 && err != nil {
			if len(line) > 0 && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			c.setPartial(false)
			select {
			case <-c.closed:
				err = io.EOF
			default:
			}
			return nil, err
		}
		if n == 0 {
			continue
		}
		if one[0] == '\n' {
			c.setPartial(false)
			return line, nil
		}
		line = append(line, one[0])
		if len(line) > MaxFrame {
			c.setPartial(false)
			return nil, errFrameTooLarge
		}
		c.setPartial(true)
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
		c.state.InitializeParams = append(json.RawMessage(nil), req.Params...)
		c.initID, c.hasInit = req.ID, true
	case "notifications/initialized":
		c.state.Initialized = true
	case "logging/setLevel":
		c.state.SetLevelParams = append(json.RawMessage(nil), req.Params...)
	}
}

// Write writes msg; a response leaves the pending set only after the bytes are
// written.
func (c *Conn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resp, isResp := msg.(*jsonrpc.Response)

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if isResp {
		c.smu.Lock()
		c.inflight++
		c.smu.Unlock()
		defer func() {
			c.smu.Lock()
			c.inflight--
			delete(c.pending, resp.ID)
			c.changed()
			c.smu.Unlock()
		}()
		if b, ok := c.takeBatch(resp); ok {
			if len(b) == 0 {
				return nil
			}
			return c.writeBatch(b)
		}
	}
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return fmt.Errorf("marshaling message: %v", err)
	}
	if _, err := c.out.Write(append(data, '\n')); err != nil {
		return err
	}
	if isResp {
		c.captureResponse(resp)
	}
	return nil
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

func (c *Conn) captureResponse(resp *jsonrpc.Response) {
	c.smu.Lock()
	defer c.smu.Unlock()
	if !c.hasInit || resp.ID != c.initID || resp.Error != nil {
		return
	}
	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(resp.Result, &r) == nil {
		c.state.ProtocolVersion = r.ProtocolVersion
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
	return !c.partial && len(c.pending) == 0 && c.inflight == 0 && len(c.queue) == 0
}

// WaitQuiescent blocks until Quiescent or ctx ends.
func (c *Conn) WaitQuiescent(ctx context.Context) error {
	for {
		c.smu.Lock()
		q := !c.partial && len(c.pending) == 0 && c.inflight == 0 && len(c.queue) == 0
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
