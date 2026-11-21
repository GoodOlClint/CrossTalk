package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

var keep = []string{"id", "ephemeral", "parentThreadId", "sessionId"}

// kinds reduces a thread source to a category and a helper subtype. Names, paths and free text inside the source are dropped.
func kinds(raw json.RawMessage) (kind, sub string) {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return word(str), ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return "other", ""
	}
	for k, v := range obj {
		kind = word(k)
		if k != "subAgent" {
			return kind, ""
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			return kind, word(s)
		}
		var inner map[string]json.RawMessage
		if json.Unmarshal(v, &inner) == nil {
			for ik := range inner {
				return kind, word(ik)
			}
		}
		return kind, ""
	}
	return "other", ""
}

func word(s string) string {
	if len(s) == 0 || len(s) > 24 {
		return "other"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_') {
			return "other"
		}
	}
	return s
}

func pick(thread map[string]json.RawMessage) map[string]any {
	out := map[string]any{}
	for _, k := range keep {
		if v, ok := thread[k]; ok {
			out[k] = v
		}
	}
	if raw, ok := thread["source"]; ok {
		out["sourceKind"], out["subAgentKind"] = kinds(raw)
	}
	var st struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(thread["status"], &st) == nil && st.Type != "" {
		out["statusType"] = word(st.Type)
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
