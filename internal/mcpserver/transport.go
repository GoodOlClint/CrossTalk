package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MaxFrame bounds one inbound frame, the same cap as the SDK's stdio transport.
const MaxFrame = mcp.DefaultMaxLineLength

// batchProtocol is the first protocol version that forbids JSON-RPC batches.
const batchProtocol = "2025-06-18"

// defaultProtocol is what the SDK assumes before a version is negotiated.
const defaultProtocol = "2025-03-26"

// methodListen is the SEP-2575 request a client keeps open to receive list
// changes; the server answers it only when the subscription ends.
const methodListen = "subscriptions/listen"

const notificationListenAck = "notifications/subscriptions/acknowledged"

// metaSubscriptionID is the _meta key that ties a notification to its listen.
const metaSubscriptionID = "io.modelcontextprotocol/subscriptionId"

var errFrameTooLarge = errors.New("inbound JSON-RPC frame exceeded the maximum line length")

// SessionState is what the Conn saw on the wire: the client's initialize
// params, the protocol version in the server's initialize response, whether
// notifications/initialized arrived, and the last logging/setLevel params.
type SessionState struct {
	InitializeParams json.RawMessage `json:"initialize_params,omitempty"`
	ProtocolVersion  string          `json:"protocol_version,omitempty"`
	Initialized      bool            `json:"initialized,omitempty"`
	SetLevelParams   json.RawMessage `json:"set_level_params,omitempty"`
	// Listens are the open subscriptions/listen requests, as read.
	Listens []json.RawMessage `json:"listens,omitempty"`
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
	listens  map[jsonrpc.ID]json.RawMessage // open subscriptions/listen requests; never block quiescence
	replayed map[string]bool                // listen ids, as JSON, whose repeated acknowledgement is dropped
	cand     map[jsonrpc.ID]candidate       // state-changing requests until their response succeeds
	werr     error                          // a failed response write; handoff must not proceed
	batches  map[jsonrpc.ID]*batch
	queue    []jsonrpc.Message
	inflight int // responses being written
	state    SessionState
	hasInit  bool
	partial  bool
	rd       chan frameRes // the outstanding frame read, if a Read gave up on it
	reading  bool          // readFrame is inside in.Read at a frame boundary
	pausing  bool          // the reader stops at the next frame boundary
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
		listens: map[jsonrpc.ID]json.RawMessage{},
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
// consumed. Close closes the input, which unblocks a Read in progress. While
// pausing it waits at a frame boundary instead of reading.
func (c *Conn) readFrame() ([]byte, error) {
	var line []byte
	var one [1]byte
	for {
		if len(line) == 0 && !c.boundary() {
			return nil, io.EOF
		}
		n, err := c.in.Read(one[:])
		if len(line) == 0 {
			c.smu.Lock()
			c.reading = false
			if n > 0 && one[0] != '\n' {
				c.partial = true
			}
			c.changed()
			c.smu.Unlock()
		}
		if n == 0 && errors.Is(err, os.ErrDeadlineExceeded) {
			if len(line) > 0 {
				c.setDeadline(time.Time{})
			}
			continue
		}
		if n == 0 && err != nil {
			select {
			case <-c.closed:
				err = io.EOF
			default:
			}
			if len(line) > 0 && errors.Is(err, io.EOF) {
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
		line = append(line, one[0])
		if len(line) > MaxFrame {
			c.setPartial(false)
			return nil, errFrameTooLarge
		}
	}
}

// boundary waits out a pause and marks the next read as one at a frame
// boundary; it reports false once the Conn is closed.
func (c *Conn) boundary() bool {
	for {
		c.smu.Lock()
		if !c.pausing {
			c.reading = true
			c.changed()
			c.smu.Unlock()
			return true
		}
		ch := c.idle
		c.smu.Unlock()
		select {
		case <-ch:
		case <-c.closed:
			return false
		}
	}
}

func (c *Conn) setDeadline(t time.Time) error {
	d, ok := c.in.(interface{ SetReadDeadline(time.Time) error })
	if !ok {
		return errNoPause
	}
	return d.SetReadDeadline(t)
}

var errNoPause = errors.New("input cannot be paused: no read deadline")

// Pause stops the reader at the next frame boundary; a read waiting for the
// first byte of a frame is interrupted by a read deadline and consumes
// nothing. The input must support read deadlines.
func (c *Conn) Pause() error {
	c.smu.Lock()
	c.pausing = true
	c.changed()
	c.smu.Unlock()
	if err := c.setDeadline(time.Now()); err != nil {
		c.Unpause()
		return err
	}
	return nil
}

// Unpause lets the reader read again.
func (c *Conn) Unpause() {
	c.setDeadline(time.Time{})
	c.smu.Lock()
	c.pausing = false
	c.changed()
	c.smu.Unlock()
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
	m, err := c.decode(line)
	if err != nil {
		c.setPartial(false)
	}
	return m, err
}

func (c *Conn) decode(line []byte) (jsonrpc.Message, error) {
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
		if req.Method == methodListen {
			if raw, err := jsonrpc.EncodeMessage(req); err == nil {
				c.listens[req.ID] = raw
			}
		} else {
			c.pending[req.ID] = struct{}{}
		}
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
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == notificationListenAck && c.takeReplayedAck(req.Params) {
		return nil
	}

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
			delete(c.listens, resp.ID)
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

// takeReplayedAck reports whether params acknowledge a listen Restore
// replayed; the client already has that acknowledgement from the previous
// image, so it is dropped once.
func (c *Conn) takeReplayedAck(params json.RawMessage) bool {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	k := string(p.Meta[metaSubscriptionID])
	c.smu.Lock()
	defer c.smu.Unlock()
	if !c.replayed[k] {
		return false
	}
	delete(c.replayed, k)
	return true
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
	s.Listens = nil
	for _, raw := range c.listens {
		s.Listens = append(s.Listens, append(json.RawMessage(nil), raw...))
	}
	slices.SortFunc(s.Listens, func(a, b json.RawMessage) int { return bytes.Compare(a, b) })
	return s
}

// Restore sets the state a refreshed process inherits, so the next refresh
// carries it on and the batch rule follows the negotiated version. Each
// carried listen is queued as if just read, so the server takes the stream
// over under the same request id; its acknowledgement is not written again.
func (c *Conn) Restore(s SessionState) {
	c.smu.Lock()
	defer c.smu.Unlock()
	listens := s.Listens
	s.Listens = nil
	c.state = s
	c.hasInit = len(s.InitializeParams) > 0
	for _, raw := range listens {
		m, err := jsonrpc.DecodeMessage(raw)
		req, ok := m.(*jsonrpc.Request)
		if err != nil || !ok || req.Method != methodListen || !req.ID.IsValid() {
			continue
		}
		id, err := json.Marshal(req.ID.Raw())
		if err != nil {
			continue
		}
		if c.replayed == nil {
			c.replayed = map[string]bool{}
		}
		c.replayed[string(id)] = true
		c.listens[req.ID] = append(json.RawMessage(nil), raw...)
		c.queue = append(c.queue, req)
	}
	c.changed()
}

// Quiescent reports that no frame is half-read, no read request still owes a
// response, and no response is mid-write. An open subscriptions/listen does
// not count: it is carried in the handoff.
func (c *Conn) Quiescent() bool {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.quiescent()
}

func (c *Conn) quiescent() bool {
	return !c.partial && len(c.pending) == 0 && c.inflight == 0 && len(c.queue) == 0 && c.werr == nil
}

// WaitQuiescent blocks until Quiescent or ctx ends.
func (c *Conn) WaitQuiescent(ctx context.Context) error {
	return c.wait(ctx, c.quiescent)
}

// WaitPaused blocks until the Conn is quiescent, paused and no read is
// outstanding, so an exec loses no input byte, or ctx ends.
func (c *Conn) WaitPaused(ctx context.Context) error {
	return c.wait(ctx, func() bool { return c.pausing && !c.reading && c.quiescent() })
}

func (c *Conn) wait(ctx context.Context, done func() bool) error {
	for {
		c.smu.Lock()
		q := done()
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
