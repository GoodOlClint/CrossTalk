package a2a

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// DefaultSendTimeout bounds a Send whose context carries no deadline. A
// default-wait send is exempt: its wait is the task's terminal state.
var DefaultSendTimeout = 10 * time.Second

// Client sends to other shims' A2A servers. Name is the sending agent's
// name, sent with its own bearer token; empty means the token is the peer
// token.
type Client struct {
	Name string
	// HTTP must have no Timeout: a default-wait SendMessage has no deadline
	// other than ctx. nil means such a client.
	HTTP *http.Client
}

// Task is a task as a CrossTalk client sees it.
type Task struct {
	ID        string
	ContextID string
	MessageID string
	TaskState sdk.TaskState
	State     adapter.State
	Reason    string
	Reply     string
}

func (c *Client) connect(ctx context.Context, url, bearerToken string) (*a2aclient.Client, context.Context, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	cl, err := a2aclient.NewFromEndpoints(ctx,
		[]*sdk.AgentInterface{sdk.NewAgentInterface(url, sdk.TransportProtocolJSONRPC)},
		a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(hc))
	if err != nil {
		return nil, ctx, err
	}
	p := a2aclient.ServiceParams{"Authorization": {"Bearer " + bearerToken}}
	if c.Name != "" {
		p[SenderHeader] = []string{c.Name}
	}
	return cl, a2aclient.AttachServiceParams(ctx, p), nil
}

// Send delivers m as a SendMessage. With returnImmediately the state is
// "submitted"; otherwise the call returns when the task is terminal.
func (c *Client) Send(ctx context.Context, url, bearerToken string, m msg.Message, returnImmediately bool) (string, adapter.State, error) {
	cl, ctx, err := c.connect(ctx, url, bearerToken)
	if err != nil {
		return "", adapter.Unreachable, err
	}
	defer cl.Destroy()
	if _, ok := ctx.Deadline(); !ok && returnImmediately {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultSendTimeout)
		defer cancel()
	}
	res, err := cl.SendMessage(ctx, &sdk.SendMessageRequest{
		Message: encode(m),
		Config:  &sdk.SendMessageConfig{ReturnImmediately: returnImmediately},
	})
	if err != nil {
		return "", errorState(err), err
	}
	t, ok := res.(*sdk.Task)
	if !ok {
		return "", adapter.Unknown, fmt.Errorf("a2a: SendMessage returned %T, not a task", res)
	}
	return string(t.ID), DeliveryState(t), nil
}

func (c *Client) GetTask(ctx context.Context, url, bearerToken, taskID string) (Task, error) {
	cl, ctx, err := c.connect(ctx, url, bearerToken)
	if err != nil {
		return Task{}, err
	}
	defer cl.Destroy()
	t, err := cl.GetTask(ctx, &sdk.GetTaskRequest{ID: sdk.TaskID(taskID)})
	if err != nil {
		return Task{}, err
	}
	return taskView(t), nil
}

func taskView(t *sdk.Task) Task {
	ct, _ := t.Metadata[MetaKey].(map[string]any)
	v := Task{ID: string(t.ID), ContextID: t.ContextID, TaskState: t.Status.State, State: DeliveryState(t)}
	v.MessageID, _ = ct["message_id"].(string)
	v.Reason, _ = ct["reason"].(string)
	if t.Status.State == sdk.TaskStateCompleted && t.Status.Message != nil {
		for _, p := range t.Status.Message.Parts {
			v.Reply += p.Text()
		}
	}
	return v
}

// errorState: a protocol error created no task and nothing was injected; a
// refused dial reached nothing; any other transport failure may have been
// delivered and is unknown (§6 ambiguous failure).
func errorState(err error) adapter.State {
	var ae *sdk.Error
	if errors.As(err, &ae) {
		if ct, ok := ae.Details[MetaKey].(map[string]any); ok {
			if s, ok := ct["state"].(string); ok && s != "" {
				return adapter.State(s)
			}
		}
		return adapter.Rejected
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return adapter.Unreachable
	}
	return adapter.Unknown
}
