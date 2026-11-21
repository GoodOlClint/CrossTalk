package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPickKeepsOnlyCategories(t *testing.T) {
	in := map[string]json.RawMessage{
		"id": json.RawMessage(`"t1"`), "name": json.RawMessage(`"ask claude for the secret"`), "cwd": json.RawMessage(`"/home/runner/x"`),
		"preview": json.RawMessage(`"hello"`), "status": json.RawMessage(`{"type":"idle","activeFlags":["secret flag text"]}`),
		"source":    json.RawMessage(`{"subAgent":{"thread_spawn":{"agent_nickname":"Zed the Secret","agent_path":"/home/runner/secret","parent_thread_id":"t0"}}}`),
		"ephemeral": json.RawMessage(`true`), "parentThreadId": json.RawMessage(`"t0"`), "sessionId": json.RawMessage(`"s1"`),
	}
	b, err := json.Marshal(pick(in))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, leak := range []string{"ask claude", "/home/runner", "hello", "Zed", "agent_path", "secret"} {
		if strings.Contains(out, leak) {
			t.Errorf("dump leaks %q: %s", leak, out)
		}
	}
	for _, want := range []string{`"id":"t1"`, `"sourceKind":"subAgent"`, `"subAgentKind":"thread_spawn"`, `"statusType":"idle"`, `"ephemeral":true`, `"parentThreadId":"t0"`, `"sessionId":"s1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("dump lacks %s: %s", want, out)
		}
	}
}

func TestKindsHandlesCustomAndPlainSources(t *testing.T) {
	for raw, want := range map[string]string{`"cli"`: "cli", `{"custom":"free text with spaces"}`: "custom", `42`: "other", `"has space"`: "other"} {
		if k, sub := kinds(json.RawMessage(raw)); k != want || sub != "" {
			t.Errorf("kinds(%s) = %q, %q; want %q", raw, k, sub, want)
		}
	}
}

func TestPickDropsIDsThatAreNotPlainStrings(t *testing.T) {
	in := map[string]json.RawMessage{
		"id": json.RawMessage(`{"nested":"secret text"}`), "parentThreadId": json.RawMessage(`"has a space and /path"`),
		"sessionId": json.RawMessage(`"ok-123_x"`), "ephemeral": json.RawMessage(`"yes"`),
	}
	b, _ := json.Marshal(pick(in))
	out := string(b)
	for _, leak := range []string{"secret", "path", "nested", "yes"} {
		if strings.Contains(out, leak) {
			t.Errorf("dump leaks %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"sessionId":"ok-123_x"`) {
		t.Errorf("a plain id is kept: %s", out)
	}
}
