#!/usr/bin/env python3 -I
# Drives a scratch `codex app-server` (stdio, own CODEX_HOME) to see what
# happens to a stdio MCP server that re-execs itself or exits.
import json, os, subprocess, sys, threading, time, queue

mode, home, log = sys.argv[1], sys.argv[2], sys.argv[3]
env = dict(os.environ, CODEX_HOME=home)
p = subprocess.Popen(["codex", "app-server"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=open(log + ".stderr", "w"), env=env)
out = open(log, "w")
q = queue.Queue()


def reader():
    for line in p.stdout:
        msg = json.loads(line)
        out.write("IN  " + json.dumps(msg)[:900] + "\n"); out.flush()
        q.put(msg)


threading.Thread(target=reader, daemon=True).start()
n = 0


def call(method, params, timeout=30):
    global n
    n += 1
    i = f"d-{n}"
    req = {"jsonrpc": "2.0", "id": i, "method": method, "params": params}
    out.write("OUT " + json.dumps(req) + "\n"); out.flush()
    p.stdin.write((json.dumps(req) + "\n").encode()); p.stdin.flush()
    end = time.time() + timeout
    while time.time() < end:
        try:
            m = q.get(timeout=end - time.time())
        except queue.Empty:
            break
        if m.get("id") == i:
            return m
    return {"timeout": method}


def note(s):
    out.write("NOTE " + s + "\n"); out.flush()


call("initialize", {"clientInfo": {"name": "upgrade-refresh-probe", "version": "1"}, "capabilities": {"experimentalApi": False}})
p.stdin.write(b'{"jsonrpc":"2.0","method":"initialized"}\n'); p.stdin.flush()
t = call("thread/start", {"cwd": os.path.dirname(log), "ephemeral": True})
tid = t["result"]["thread"]["id"]
time.sleep(3)

def tools():
    r = call("mcpServerStatus/list", {"detail": "toolsAndAuthOnly"})
    for s in r.get("result", {}).get("data", []):
        if s["name"] == "probe":
            return sorted(s.get("tools", {}).keys()) if isinstance(s.get("tools"), dict) else s.get("tools")
    return r

def tc(tool):
    r = call("mcpServer/tool/call", {"threadId": tid, "server": "probe", "tool": tool, "arguments": {}})
    return r.get("result", r.get("error", r))

note(f"tools before: {tools()}")
note(f"ping 1: {tc('probe_ping')}")
note(f"restart: {tc('probe_restart')}")
time.sleep(3)
note(f"ping 2: {tc('probe_ping')}")
note(f"tools after: {tools()}")
note(f"probe_new: {tc('probe_new')}")
t2 = call("thread/start", {"cwd": os.path.dirname(log), "ephemeral": True})
tid = t2["result"]["thread"]["id"]
time.sleep(3)
note(f"new thread ping: {tc('probe_ping')}")
p.terminate()
