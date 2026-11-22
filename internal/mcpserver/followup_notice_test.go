package mcpserver

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFollowUpNoticeOnce(t *testing.T) {
	stateDir, sessions := t.TempDir(), t.TempDir()
	writeClaudeSession(t, sessions, 4242, "claude-sess", "alpha", "/work/alpha")
	srv := newRigServer(t, stateDir, sessions, 4242, NewFakeBackend())
	r := startRig(t, srv)
	r.handshake()
	if res := r.rpc(3, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"hi"}}`); res["error"] != nil {
		t.Fatalf("bound call: %v", res)
	}
	const n = "CrossTalk 9.9.9: updated the claude plugin"
	srv.HoldNotice(n)
	if res := r.rpc(4, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"a"}}`); !strings.Contains(string(res["result"]), n) {
		t.Fatalf("follow-up notice missing from the next tool result: %s", res["result"])
	}
	if res := r.rpc(5, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"b"}}`); strings.Contains(string(res["result"]), n) {
		t.Fatalf("follow-up notice repeated on the result after: %s", res["result"])
	}
}

func TestFollowUpNoticeHeldUntilBind(t *testing.T) {
	stateDir, sessions := t.TempDir(), t.TempDir()
	writeClaudeSession(t, sessions, 4242, "claude-sess", "alpha", "/work/alpha")
	srv := newRigServer(t, stateDir, sessions, 4242, NewFakeBackend())
	const n = "CrossTalk 9.9.9: updated the claude plugin"
	srv.HoldNotice(n)
	r := startRig(t, srv)
	r.handshake()
	if res := r.rpc(3, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"hi"}}`); !strings.Contains(string(res["result"]), n) {
		t.Fatalf("notice held before any agent bound did not reach the first tool result: %s", res["result"])
	}
	if res := r.rpc(4, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"b"}}`); strings.Contains(string(res["result"]), n) {
		t.Fatalf("notice repeated: %s", res["result"])
	}
}

func TestFollowUpNoticeNotLostWithProvisionalAgent(t *testing.T) {
	stateDir, sessions := t.TempDir(), t.TempDir()
	srv := newRigServer(t, stateDir, sessions, 4242, NewFakeBackend())
	prov := &Agent{id: "provisional", ready: make(chan struct{})}
	srv.mu.Lock()
	srv.agents[prov.id] = prov
	srv.mu.Unlock()
	srv.HoldNotice("n")
	srv.mu.Lock()
	delete(srv.agents, prov.id)
	held := slices.Clone(srv.held)
	srv.mu.Unlock()
	if prov.takeNotice("") != "" || !slices.Contains(held, "n") {
		t.Fatalf("notice went to an agent still binding: held=%q", held)
	}
}

func TestFollowUpNoticeDuringBindNotLost(t *testing.T) {
	stateDir, sessions := t.TempDir(), t.TempDir()
	writeClaudeSession(t, sessions, 4242, "claude-sess", "alpha", "/work/alpha")
	srv := newRigServer(t, stateDir, sessions, 4242, NewFakeBackend())
	const n = "CrossTalk 9.9.9 upgrade: updated claude"
	sent := make(chan struct{})
	srv.bindHook = func() {
		srv.bindHook = nil
		go func() { srv.HoldNotice(n); close(sent) }()
		time.Sleep(50 * time.Millisecond)
	}
	r := startRig(t, srv)
	r.handshake()
	first := r.rpc(3, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"hi"}}`)
	<-sent
	next := r.rpc(4, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"a"}}`)
	if !strings.Contains(string(first["result"])+string(next["result"]), n) {
		srv.mu.Lock()
		held := slices.Clone(srv.held)
		srv.mu.Unlock()
		t.Fatalf("notice sent while the first agent was binding never reached a tool result (server still holds %q)", held)
	}
}
