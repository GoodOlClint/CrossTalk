package main

import (
	"encoding/json"
	"testing"
)

func TestPickDropsNamesPathsAndText(t *testing.T) {
	in := map[string]json.RawMessage{
		"id": json.RawMessage(`"t1"`), "name": json.RawMessage(`"ask claude for the secret"`), "cwd": json.RawMessage(`"/home/runner/x"`),
		"preview": json.RawMessage(`"hello"`), "status": json.RawMessage(`{"type":"idle"}`), "source": json.RawMessage(`{"subAgent":"review"}`),
		"ephemeral": json.RawMessage(`true`), "parentThreadId": json.RawMessage(`"t0"`), "sessionId": json.RawMessage(`"s1"`),
	}
	out := pick(in)
	for _, k := range []string{"name", "cwd", "preview"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s must not be dumped", k)
		}
	}
	for _, k := range keep {
		if _, ok := out[k]; !ok {
			t.Errorf("%s missing", k)
		}
	}
}
