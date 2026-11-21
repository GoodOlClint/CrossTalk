package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

var keep = []string{"id", "status", "source", "ephemeral", "parentThreadId", "sessionId"}

func pick(thread map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, k := range keep {
		if v, ok := thread[k]; ok {
			out[k] = v
		}
	}
	return out
}

func run(ctx context.Context, endpoint string) error {
	c, err := appserver.Dial(ctx, endpoint, appserver.Options{})
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Initialize(ctx, appserver.ClientInfo{Name: "harness-compat-threaddump", Version: "0"}); err != nil {
		return err
	}
	ids, err := c.LoadedThreads(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, id := range ids {
		raw, err := c.CallRaw(ctx, "thread/read", map[string]any{"threadId": id})
		if err != nil {
			_ = enc.Encode(map[string]string{"id": id, "error": err.Error()})
			continue
		}
		var r struct {
			Thread map[string]json.RawMessage `json:"thread"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			_ = enc.Encode(map[string]string{"id": id, "error": err.Error()})
			continue
		}
		_ = enc.Encode(pick(r.Thread))
	}
	return nil
}

func main() {
	endpoint := ""
	if len(os.Args) > 1 {
		endpoint = os.Args[1]
	} else {
		var err error
		if endpoint, err = appserver.DefaultControlSocket(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := run(ctx, endpoint); err != nil {
		fmt.Fprintln(os.Stderr, "threaddump:", err)
		os.Exit(1)
	}
}
