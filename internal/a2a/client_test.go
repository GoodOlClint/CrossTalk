package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func plain(text string, k msg.Kind) msg.Message {
	return msg.Message{MessageID: msg.NewUUIDv7(), Kind: k, Delivery: msg.Immediate, Parts: []msg.Part{{Type: "text", Text: text}}}
}

func TestClientSend(t *testing.T) {
	e := newEnv(t, receiver.Limits{}, Limits{}, nil)
	xTok := e.addAgent("x", "xxxxxxxxxxxxxxxx", registry.RolePrimary)
	ctx := context.Background()
	x := &Client{Name: "x"}

	id, st, err := x.Send(ctx, e.url, xTok, plain("inform", msg.Inform), false)
	if err != nil || id == "" || st != adapter.Accepted {
		t.Fatalf("default wait: %q %s %v", id, st, err)
	}
	if w := e.fake.Writes(); len(w) != 1 || w[0].Message.From != "x" {
		t.Fatalf("writes %+v", w)
	}

	ask := plain("ask", msg.Ask)
	id, st, err = x.Send(ctx, e.url, xTok, ask, true)
	if err != nil || st != journal.StateSubmitted {
		t.Fatalf("immediate: %s %v", st, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, err := x.GetTask(ctx, e.url, xTok, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.TaskState == sdk.TaskStateWorking {
			if task.State != adapter.Accepted || task.MessageID != ask.MessageID {
				t.Fatalf("working ask %+v", task)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ask never working: %+v", task)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.srv.CompleteAsk(ask.MessageID, "42"); err != nil {
		t.Fatal(err)
	}
	task, err := x.GetTask(ctx, e.url, xTok, id)
	if err != nil || task.TaskState != sdk.TaskStateCompleted || task.Reply != "42" || task.State != adapter.Accepted {
		t.Fatalf("completed ask %+v %v", task, err)
	}

	peer := &Client{}
	_, st, err = peer.Send(ctx, e.url, e.peerTok, plain("command", msg.Command), false)
	if err != nil || st != adapter.Rejected {
		t.Fatalf("command from local-user: %s %v", st, err)
	}

	e.fake.SetDefaultInject(adapter.Outcome{State: adapter.Unreachable, NoWrite: true})
	_, st, err = peer.Send(ctx, e.url, e.peerTok, plain("nobody home", msg.Inform), false)
	if err != nil || st != adapter.Unreachable {
		t.Fatalf("unreachable: %s %v", st, err)
	}

	_, st, err = peer.Send(ctx, e.url, "wrong", plain("hi", msg.Inform), false)
	if !errors.Is(err, sdk.ErrUnauthenticated) || st != adapter.Rejected {
		t.Fatalf("bad token: %s %v", st, err)
	}
	if _, err := x.GetTask(ctx, e.url, e.peerTok, id); !errors.Is(err, sdk.ErrUnauthenticated) {
		t.Fatalf("peer token under agent name: %v", err)
	}
	if _, err := peer.GetTask(ctx, e.url, e.peerTok, id); !errors.Is(err, sdk.ErrTaskNotFound) {
		t.Fatalf("other principal's task: %v", err)
	}
}

func TestClientUnreachableServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	ln.Close()
	_, st, err := (&Client{}).Send(context.Background(), url, "tok", plain("hi", msg.Inform), true)
	if err == nil || st != adapter.Unreachable {
		t.Fatalf("closed port: %s %v", st, err)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	irt := msg.NewUUIDv7()
	tid := "task-1"
	in := msg.Message{MessageID: msg.NewUUIDv7(), InReplyTo: &irt, ContextID: "ctx", TaskID: &tid, Kind: msg.Ask, Delivery: msg.NextTurn, DeadlineSeconds: 30, HopChain: []string{msg.Hop("a"), msg.Hop("b")}, Parts: []msg.Part{{Type: "text", Text: "q"}}}
	am := encode(in)
	if am.ID != in.MessageID || string(am.TaskID) != tid || am.ContextID != "ctx" {
		t.Fatalf("encoded %+v", am)
	}
	raw, err := json.Marshal(am)
	if err != nil {
		t.Fatal(err)
	}
	var wire sdk.Message
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	out, err := decode(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if out.MessageID != in.MessageID || *out.InReplyTo != irt || out.Kind != msg.Ask || out.Delivery != msg.NextTurn || out.DeadlineSeconds != 30 || len(out.HopChain) != 2 || out.Parts[0].Text != "q" || out.ContextID != "ctx" {
		t.Fatalf("decoded %+v", out)
	}
	plainMsg := &sdk.Message{ID: "not-a-uuid", Role: sdk.MessageRoleUser, Parts: sdk.ContentParts{sdk.NewTextPart("hi")}}
	d, err := decode(plainMsg)
	if err != nil || d.Kind != msg.Inform || d.Delivery != msg.Immediate || !msg.IsUUIDv7(d.MessageID) {
		t.Fatalf("plain %+v %v", d, err)
	}
	if _, err := decode(&sdk.Message{ID: "x", Parts: sdk.ContentParts{sdk.NewDataPart(map[string]any{"a": 1})}}); !errors.Is(err, sdk.ErrInvalidParams) {
		t.Fatalf("data part: %v", err)
	}
}
