package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"syscall"
	"time"
)

type Kind int

const (
	KindUnreachable Kind = iota + 1
	KindNotFound
	KindUnknown
)

func (k Kind) String() string {
	switch k {
	case KindUnreachable:
		return "unreachable"
	case KindNotFound:
		return "not-found"
	case KindUnknown:
		return "unknown"
	}
	return "invalid"
}

type Error struct {
	Kind   Kind
	Status int
	Msg    string
	Err    error
}

func (e *Error) Error() string {
	s := "opencode " + e.Kind.String() + ": " + e.Msg
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

type Response struct {
	Status int
	Body   []byte
}

const maxBody = 16 << 20

const defaultTimeout = 30 * time.Second

type Client struct {
	ServicePath string
	Timeout     time.Duration
	PinOrigin   string
	http        *http.Client
}

func NewClient(servicePath string) *Client {
	if servicePath == "" {
		servicePath = DefaultServicePath()
	}
	return &Client{
		ServicePath: servicePath,
		Timeout:     defaultTimeout,
		http: &http.Client{
			Transport:     &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

var sessionID = regexp.MustCompile(`^ses_[A-Za-z0-9]+$`)

func (c *Client) Sessions(ctx context.Context) (*Response, error) {
	return c.do(ctx, http.MethodGet, "/api/session", nil)
}

func (c *Client) ActiveSessions(ctx context.Context) (*Response, error) {
	return c.do(ctx, http.MethodGet, "/api/session/active", nil)
}

func (c *Client) Session(ctx context.Context, id string) (*Response, error) {
	if !sessionID.MatchString(id) {
		return nil, badID()
	}
	return c.do(ctx, http.MethodGet, "/api/session/"+id, nil)
}

func (c *Client) Prompt(ctx context.Context, id string, body []byte) (*Response, error) {
	if !sessionID.MatchString(id) {
		return nil, badID()
	}
	return c.do(ctx, http.MethodPost, "/api/session/"+id+"/prompt", body)
}

func badID() error {
	return &Error{Kind: KindUnknown, Msg: "session id is not of the form ses_<alphanumeric>; nothing sent"}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*Response, error) {
	svc, err := ReadService(c.ServicePath)
	if err != nil {
		return nil, &Error{Kind: KindUnreachable, Msg: "service file refused; nothing sent", Err: err}
	}
	if c.PinOrigin != "" && svc.Origin != c.PinOrigin {
		return nil, &Error{Kind: KindUnreachable, Msg: "service origin differs from the pinned origin; nothing sent"}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, svc.Origin+path, rd)
	if err != nil {
		return nil, &Error{Kind: KindUnknown, Msg: "request not built"}
	}
	req.SetBasicAuth("opencode", svc.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, &Error{Kind: KindUnreachable, Msg: "connection refused"}
		}
		msg := "request failed after send"
		if ctx.Err() != nil {
			msg = "request timed out or was canceled"
		}
		return nil, &Error{Kind: KindUnknown, Msg: msg}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, &Error{Kind: KindUnreachable, Status: 401, Msg: "service refused the credential"}
	case http.StatusNotFound:
		return nil, &Error{Kind: KindNotFound, Status: 404, Msg: "not found"}
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(out) > maxBody {
		return nil, &Error{Kind: KindUnknown, Status: resp.StatusCode, Msg: "response body unreadable or too large"}
	}
	switch {
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &Error{Kind: KindUnknown, Status: resp.StatusCode, Msg: fmt.Sprintf("status %d", resp.StatusCode)}
	case !json.Valid(out):
		return nil, &Error{Kind: KindUnknown, Status: resp.StatusCode, Msg: "success response is not JSON"}
	}
	return &Response{Status: resp.StatusCode, Body: out}, nil
}
