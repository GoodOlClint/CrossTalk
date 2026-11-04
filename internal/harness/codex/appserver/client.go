package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ControlSocketRel is the managed daemon socket, relative to the home directory.
const ControlSocketRel = ".codex/app-server-control/app-server-control.sock"

// WriteTimeout bounds every websocket write, including the close frame.
var WriteTimeout = 5 * time.Second

var (
	// ErrNotSent: the request never fully reached the server; nothing was delivered.
	ErrNotSent = errors.New("appserver: request not sent")
	// ErrAmbiguous: the request was written but no response arrived (disconnect,
	// timeout, cancellation); the server may or may not have acted on it.
	ErrAmbiguous   = errors.New("appserver: no response to sent request")
	ErrBadEndpoint = errors.New("appserver: unsupported endpoint")
)

// DefaultControlSocket returns the managed daemon socket path under $HOME.
func DefaultControlSocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ControlSocketRel), nil
}

// NormalizeEndpoint returns the canonical form of an endpoint: unix://<abs
// path> for a socket path (bare, ~-prefixed or unix://), or the ws:// URL.
func NormalizeEndpoint(ep string) (string, error) {
	switch {
	case strings.HasPrefix(ep, "ws://"):
		u, err := url.Parse(ep)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("%w: %q", ErrBadEndpoint, ep)
		}
		return ep, nil
	case strings.HasPrefix(ep, "unix://"):
		ep = strings.TrimPrefix(ep, "unix://")
	case strings.Contains(ep, "://"):
		return "", fmt.Errorf("%w: %q", ErrBadEndpoint, ep)
	}
	if ep == "~" || strings.HasPrefix(ep, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		ep = filepath.Join(home, strings.TrimPrefix(ep, "~"))
	}
	if !filepath.IsAbs(ep) {
		return "", fmt.Errorf("%w: socket path must be absolute: %q", ErrBadEndpoint, ep)
	}
	return "unix://" + filepath.Clean(ep), nil
}

// RPCError is a JSON-RPC error response from the server.
type RPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("appserver: rpc error %d: %s (data %s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("appserver: rpc error %d: %s", e.Code, e.Message)
}

// Frame directions passed to Options.OnFrame.
const (
	DirOut = "out"
	DirIn  = "in"
)

type Options struct {
	// OnFrame sees every JSON text frame written or read, unmodified.
	OnFrame func(dir string, raw []byte)
	// OnNotification sees server notifications (no id).
	OnNotification func(method string, params json.RawMessage)
}

type response struct {
	result json.RawMessage
	err    *RPCError
}

type Client struct {
	conn     net.Conn
	br       *bufio.Reader
	opt      Options
	endpoint string

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan response
	readErr error
	done    chan struct{}

	init InitializeResult
}

// Dial connects to endpoint and completes the websocket upgrade. Call
// Initialize before any other method (F3).
func Dial(ctx context.Context, endpoint string, opt Options) (*Client, error) {
	ep, err := NormalizeEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	var conn net.Conn
	host, path := "localhost", "/"
	if sock, ok := strings.CutPrefix(ep, "unix://"); ok {
		conn, err = d.DialContext(ctx, "unix", sock)
	} else {
		u, _ := url.Parse(ep)
		host, path = u.Host, u.RequestURI()
		conn, err = d.DialContext(ctx, "tcp", u.Host)
	}
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	br, err := ClientHandshake(conn, host, path)
	if !stop() {
		conn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	c := &Client{conn: conn, br: br, opt: opt, endpoint: ep, pending: map[string]chan response{}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

// Endpoint is the normalized endpoint this client is connected to.
func (c *Client) Endpoint() string { return c.endpoint }

func (c *Client) Close() error {
	c.wmu.Lock()
	c.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	_ = WriteFrame(c.conn, OpClose, nil, true)
	c.wmu.Unlock()
	err := c.conn.Close()
	<-c.done
	return err
}

// Done is closed when the connection's read side ends.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) write(raw []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.opt.OnFrame != nil {
		c.opt.OnFrame(DirOut, raw)
	}
	return c.writeFrame(OpText, raw)
}

func (c *Client) writeFrame(op byte, payload []byte) error {
	c.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	err := WriteFrame(c.conn, op, payload, true)
	if err != nil {
		c.conn.Close()
	}
	return err
}

type inbound struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

func idKey(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return "s:" + s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return "n:" + n.String()
	}
	return "r:" + string(raw)
}

func (c *Client) readLoop() {
	defer close(c.done)
	control := func(op byte, payload []byte) error {
		if op != OpPing {
			return nil
		}
		c.wmu.Lock()
		defer c.wmu.Unlock()
		return c.writeFrame(OpPong, payload)
	}
	var err error
	for {
		var op byte
		var data []byte
		op, data, err = ReadMessage(c.br, control)
		if err != nil {
			break
		}
		if op != OpText {
			continue
		}
		if c.opt.OnFrame != nil {
			c.opt.OnFrame(DirIn, data)
		}
		var m inbound
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		hasID := len(m.ID) > 0 && string(m.ID) != "null"
		switch {
		case m.Method != "" && hasID:
			c.refuseServerRequest(m.ID)
		case m.Method != "":
			if c.opt.OnNotification != nil {
				c.opt.OnNotification(m.Method, m.Params)
			}
		case hasID:
			c.mu.Lock()
			ch, ok := c.pending[idKey(m.ID)]
			delete(c.pending, idKey(m.ID))
			c.mu.Unlock()
			if ok {
				ch <- response{result: m.Result, err: m.Error}
			}
		}
	}
	c.mu.Lock()
	c.readErr = err
	for k, ch := range c.pending {
		close(ch)
		delete(c.pending, k)
	}
	c.mu.Unlock()
}

func (c *Client) refuseServerRequest(id json.RawMessage) {
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32601, "message": "crosstalk does not serve server requests"},
	})
	_ = c.write(raw)
}

// Call sends a request and decodes the result into out (if non-nil). Errors:
// *RPCError for an error response; ErrNotSent when nothing was delivered;
// ErrAmbiguous when the request was written but no response arrived.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	raw, err := c.CallRaw(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("appserver: decode %s result: %w", method, err)
	}
	return nil
}

func (c *Client) CallRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = struct{}{}
	}
	c.mu.Lock()
	if c.readErr != nil || isClosed(c.done) {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s: connection closed", ErrNotSent, method)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[idKey(json.RawMessage(strconv.FormatInt(id, 10)))] = ch
	c.mu.Unlock()

	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err == nil {
		err = c.write(raw)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, idKey(json.RawMessage(strconv.FormatInt(id, 10))))
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s: %v", ErrNotSent, method, err)
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%w: %s: connection closed", ErrAmbiguous, method)
		}
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, idKey(json.RawMessage(strconv.FormatInt(id, 10))))
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s: %v", ErrAmbiguous, method, ctx.Err())
	}
}

// Notify sends a notification (no id, no response).
func (c *Client) Notify(method string, params any) error {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := c.write(raw); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrNotSent, method, err)
	}
	return nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
