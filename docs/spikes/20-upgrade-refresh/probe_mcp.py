#!/usr/bin/env python3 -I
# Scratch probe for the upgrade-refresh design: a stdio MCP server that can
# re-exec itself in place (PROBE_MODE=exec) or exit (PROBE_MODE=exit) after
# answering probe_restart. Gen 2 restores the initialized state from the env
# and announces a new tool with notifications/tools/list_changed.
import json, os, sys, time

GEN = int(os.environ.get("PROBE_GEN", "1"))
MODE = os.environ.get("PROBE_MODE", "exec")
LOG = os.environ.get("PROBE_LOG", "/tmp/upgrade-refresh-probe.log")
initialized = os.environ.get("PROBE_INIT") == "1"


def log(*a):
    with open(LOG, "a") as f:
        f.write(f"{time.strftime('%H:%M:%S')} pid={os.getpid()} ppid={os.getppid()} gen={GEN} " + " ".join(map(str, a)) + "\n")


def readline():
    # unbuffered, so no bytes of a following message are read ahead and lost on exec
    b = bytearray()
    while True:
        c = os.read(0, 1)
        if not c:
            return None
        if c == b"\n":
            return bytes(b)
        b += c


def send(obj):
    os.write(1, (json.dumps(obj) + "\n").encode())


def tools():
    t = [
        {"name": "probe_ping", "description": "Returns the probe's pid and generation.", "inputSchema": {"type": "object", "properties": {}}},
        {"name": "probe_restart", "description": "Restarts the probe server (re-exec in place or exit).", "inputSchema": {"type": "object", "properties": {}}},
    ]
    if GEN >= 2:
        t.append({"name": "probe_new", "description": "Exists only after a restart.", "inputSchema": {"type": "object", "properties": {}}})
    return t


log("start", "mode=" + MODE, "initialized=" + str(initialized))
if GEN >= 2 and initialized:
    send({"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
    log("sent tools/list_changed")

while True:
    line = readline()
    if line is None:
        log("stdin EOF, exiting")
        sys.exit(0)
    if not line.strip():
        continue
    msg = json.loads(line)
    m, i = msg.get("method"), msg.get("id")
    log("recv", m, "id=" + str(i))
    if m == "initialize":
        initialized = True
        send({"jsonrpc": "2.0", "id": i, "result": {"protocolVersion": msg["params"].get("protocolVersion", "2025-06-18"), "capabilities": {"tools": {"listChanged": True}}, "serverInfo": {"name": "probe", "version": str(GEN)}}})
    elif m == "tools/list":
        send({"jsonrpc": "2.0", "id": i, "result": {"tools": tools()}})
        log("listed", [t["name"] for t in tools()])
    elif m == "tools/call":
        name = msg["params"]["name"]
        if not initialized:
            send({"jsonrpc": "2.0", "id": i, "error": {"code": -32600, "message": "not initialized"}})
            continue
        text = f"pid={os.getpid()} gen={GEN} tool={name}"
        send({"jsonrpc": "2.0", "id": i, "result": {"content": [{"type": "text", "text": text}]}})
        if name == "probe_restart":
            if MODE == "exec":
                env = dict(os.environ, PROBE_GEN=str(GEN + 1), PROBE_INIT="1")
                log("execv in place")
                os.execve(sys.executable, [sys.executable, "-I", os.path.abspath(__file__)], env)
            else:
                log("exit(0) after restart")
                sys.exit(0)
    elif m == "ping":
        send({"jsonrpc": "2.0", "id": i, "result": {}})
    elif i is not None:
        send({"jsonrpc": "2.0", "id": i, "error": {"code": -32601, "message": "method not found"}})
