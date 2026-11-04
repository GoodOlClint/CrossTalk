package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
	// ServerVersion is parsed from UserAgent ("codex-tui/0.166.0 (...)").
	ServerVersion string `json:"-"`
}

var userAgentVersion = regexp.MustCompile(`^[^/\s]+/(\d+\.\d+\.\d+[0-9A-Za-z.+-]*)`)

// ParseServerVersion extracts the version from an initialize userAgent.
func ParseServerVersion(userAgent string) (string, bool) {
	m := userAgentVersion.FindStringSubmatch(userAgent)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Initialize performs the handshake: initialize, await its result, then the
// initialized notification (F3). experimentalApi is false (spike 9).
func (c *Client) Initialize(ctx context.Context, info ClientInfo) (InitializeResult, error) {
	var r InitializeResult
	params := map[string]any{"clientInfo": info, "capabilities": map[string]any{"experimentalApi": false}}
	if err := c.Call(ctx, "initialize", params, &r); err != nil {
		return InitializeResult{}, err
	}
	r.ServerVersion, _ = ParseServerVersion(r.UserAgent)
	if err := c.Notify("initialized", nil); err != nil {
		return InitializeResult{}, err
	}
	c.init = r
	return r, nil
}

// ServerVersion is the version captured by Initialize, or "".
func (c *Client) ServerVersion() string { return c.init.ServerVersion }

// LoadedThreads returns every thread id from thread/loaded/list, following nextCursor.
func (c *Client) LoadedThreads(ctx context.Context) ([]string, error) {
	var ids []string
	var cursor *string
	for range 1000 {
		var r struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := c.Call(ctx, "thread/loaded/list", params, &r); err != nil {
			return nil, err
		}
		ids = append(ids, r.Data...)
		if r.NextCursor == nil || *r.NextCursor == "" {
			return ids, nil
		}
		cursor = r.NextCursor
	}
	return nil, fmt.Errorf("appserver: thread/loaded/list did not terminate")
}

const (
	ThreadIdle        = "idle"
	ThreadActive      = "active"
	ThreadNotLoaded   = "notLoaded"
	ThreadSystemError = "systemError"
)

type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

type Thread struct {
	ID     string       `json:"id"`
	Name   *string      `json:"name"`
	Cwd    string       `json:"cwd"`
	Status ThreadStatus `json:"status"`
	// CanAcceptDirectInput is absent from the 0.166.0 generated schema; nil
	// means the server did not say.
	CanAcceptDirectInput *bool `json:"canAcceptDirectInput,omitempty"`
}

func (c *Client) ReadThread(ctx context.Context, threadID string) (Thread, error) {
	var r struct {
		Thread Thread `json:"thread"`
	}
	if err := c.Call(ctx, "thread/read", map[string]any{"threadId": threadID}, &r); err != nil {
		return Thread{}, err
	}
	return r.Thread, nil
}

const (
	TurnInProgress  = "inProgress"
	TurnCompleted   = "completed"
	TurnInterrupted = "interrupted"
	TurnFailed      = "failed"
)

type Turn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (c *Client) ListTurns(ctx context.Context, threadID string, limit int) ([]Turn, error) {
	var r struct {
		Data []Turn `json:"data"`
	}
	params := map[string]any{"threadId": threadID}
	if limit > 0 {
		params["limit"] = limit
	}
	if err := c.Call(ctx, "thread/turns/list", params, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// ActiveTurn returns the id of the inProgress turn, or "" when there is none.
func (c *Client) ActiveTurn(ctx context.Context, threadID string) (string, error) {
	turns, err := c.ListTurns(ctx, threadID, 5)
	if err != nil {
		return "", err
	}
	for _, t := range turns {
		if t.Status == TurnInProgress {
			return t.ID, nil
		}
	}
	return "", nil
}

type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textInput(text string) []UserInput { return []UserInput{{Type: "text", Text: text}} }

// SteerTurn calls turn/steer. The raw result is returned because the wire
// placement of steer errors is not assumed (§6 F1).
func (c *Client) SteerTurn(ctx context.Context, threadID, expectedTurnID, text string) (json.RawMessage, error) {
	return c.CallRaw(ctx, "turn/steer", map[string]any{
		"threadId": threadID, "expectedTurnId": expectedTurnID, "input": textInput(text),
	})
}

// StartTurn calls turn/start with only threadId and input, so the thread's
// existing model, cwd and approval settings are kept (§6).
func (c *Client) StartTurn(ctx context.Context, threadID, text string) (json.RawMessage, error) {
	return c.CallRaw(ctx, "turn/start", map[string]any{"threadId": threadID, "input": textInput(text)})
}

func (c *Client) McpServerStatus(ctx context.Context, threadID, serverName string) (json.RawMessage, error) {
	params := map[string]any{}
	if threadID != "" {
		params["threadId"] = threadID
	}
	if serverName != "" {
		params["serverName"] = serverName
	}
	return c.CallRaw(ctx, "mcpServerStatus/list", params)
}

type ToolOutput struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Output    string `json:"output"`
	Retain    bool   `json:"retain"`
}

func (c *Client) StartToolOutput(ctx context.Context, threadID, text string) (json.RawMessage, error) {
	return c.CallRaw(ctx, "turn/start", map[string]any{
		"threadId":   threadID,
		"input":      []UserInput{},
		"toolOutput": ToolOutput{Name: "receive_message", Namespace: "crosstalk", Output: text, Retain: true},
	})
}

// Raw outputs wait for a later model request; this call never starts a turn.
func (c *Client) InjectToolOutput(ctx context.Context, threadID, text string) (json.RawMessage, error) {
	return c.CallRaw(ctx, "thread/inject_items", map[string]any{
		"threadId": threadID,
		"items": []any{map[string]any{
			"type": "function_call_output", "name": "receive_message", "namespace": "crosstalk", "output": text,
		}},
	})
}
