package codex

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

const notSteerableKey = "activeTurnNotSteerable"

// NotSteerable reports whether a turn/steer or turn/start RPC error is the
// activeTurnNotSteerable refusal. turn/steer carries it as a key under
// error.data; turn/start carries it only in error.message, in Rust debug
// casing (ActiveTurnNotSteerable { turn_kind: Review }). Only an error can
// classify; a successful result never does.
func NotSteerable(err error) (turnKind string, ok bool) {
	var rpc *appserver.RPCError
	if !errors.As(err, &rpc) {
		return "", false
	}
	if k, ok := findKey(rpc.Data); ok {
		return k, true
	}
	if strings.Contains(strings.ToLower(rpc.Message), strings.ToLower(notSteerableKey)) {
		return "", true
	}
	return "", false
}

func findKey(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return walk(v)
}

func walk(v any) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		if inner, ok := t[notSteerableKey]; ok {
			kind := ""
			if m, ok := inner.(map[string]any); ok {
				kind, _ = m["turnKind"].(string)
			}
			return kind, true
		}
		for _, c := range t {
			if k, ok := walk(c); ok {
				return k, true
			}
		}
	case []any:
		for _, c := range t {
			if k, ok := walk(c); ok {
				return k, true
			}
		}
	case string:
		if t == notSteerableKey {
			return "", true
		}
	}
	return "", false
}
