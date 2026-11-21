#!/bin/sh
set -eu

# assert-roundtrip STATE: each direction passes only when the asker's journal
# holds a receipt (an admitted reply) for an ask it sent to the other harness.
assert_one() {
	state="$1" a="$2" b="$3"
	aid="$(jq -r --arg h "$a" 'select(.harness == $h) | .agent_id' "$state"/agents/*.json 2>/dev/null | head -n 1)"
	bid="$(jq -r --arg h "$b" 'select(.harness == $h) | .agent_id' "$state"/agents/*.json 2>/dev/null | head -n 1)"
	[ -n "$aid" ] && [ -n "$bid" ] && [ -f "$state/journal/$aid.jsonl" ] || {
		echo "FAIL $a->$b: agent or journal missing (a=$aid b=$bid)"
		return 1
	}
	asks="$(jq -r --arg to "$bid" 'select(.type == "outbound" and .kind == "ask" and .to == $to) | .message_id' "$state/journal/$aid.jsonl" | sort -u)"
	[ -n "$asks" ] || {
		echo "FAIL $a->$b: no ask sent"
		return 1
	}
	got="$(jq -r 'select(.type == "receipt") | .in_reply_to' "$state/journal/$aid.jsonl" | sort -u)"
	for m in $asks; do
		if printf '%s\n' "$got" | grep -qx "$m"; then
			echo "PASS $a->$b: reply receipt for ask $m"
			return 0
		fi
	done
	echo "FAIL $a->$b: ask sent, no reply receipt"
	return 1
}

assert_ids() { # state asker_id target_id
	state="$1" aid="$2" bid="$3"
	[ -f "$state/journal/$aid.jsonl" ] || {
		echo "FAIL $aid->$bid: no journal for the asker"
		return 1
	}
	asks="$(jq -r --arg to "$bid" 'select(.type == "outbound" and .kind == "ask" and .to == $to) | .message_id' "$state/journal/$aid.jsonl" | sort -u)"
	[ -n "$asks" ] || {
		echo "FAIL $aid->$bid: no ask sent"
		return 1
	}
	got="$(jq -r 'select(.type == "receipt") | .in_reply_to' "$state/journal/$aid.jsonl" | sort -u)"
	for m in $asks; do
		if printf '%s\n' "$got" | grep -qx "$m"; then
			echo "PASS $aid->$bid: reply receipt for ask $m"
			return 0
		fi
	done
	echo "FAIL $aid->$bid: ask sent, no reply receipt"
	return 1
}

wait_agent() {
	state="$1" h="$2" n=0
	while ! jq -e --arg h "$h" 'select(.harness == $h)' "$state"/agents/*.json >/dev/null 2>&1; do
		n=$((n + 1))
		[ "$n" -lt 90 ] || return 1
		sleep 1
	done
}

agent_ids_of() { # state harness [exclude] -> ids of bound descriptors
	cat "$1"/agents/*.json 2>/dev/null | jq -rs --arg h "$2" --arg x "${3-}" 'map(select(.harness == $h and .agent_id != $x)) | sort_by(.agent_id) | .[].agent_id'
}

case "${1:-}" in
agent-id)
	ids="$(agent_ids_of "${2:?usage: harness-compat-live.sh agent-id STATE HARNESS [EXCLUDE]}" "${3:?}" "${4-}")"
	[ "$(printf '%s\n' "$ids" | grep -c .)" -eq 1 ] || {
		echo "expected exactly one ${3} descriptor${4:+ besides $4}, got: $ids" >&2
		exit 1
	}
	printf '%s\n' "$ids"
	;;
assert-one-codex-row)
	python3 -I - "${2:?usage: harness-compat-live.sh assert-one-codex-row ROLLOUT_DIR SESSION_REF}" "${3:?}" <<'PY'
import glob, json, sys
root, ref = sys.argv[1], sys.argv[2]
files = [f for f in glob.glob(root + "/**/rollout-*.jsonl", recursive=True) if f.endswith("-" + ref + ".jsonl")]
if len(files) != 1:
    print("WAIT one-row: " + str(len(files)) + " rollout file(s) for session " + ref)
    sys.exit(2)

def agents_of(v):
    if isinstance(v, dict):
        if isinstance(v.get("agents"), list):
            return v["agents"]
        for x in v.values():
            r = agents_of(x)
            if r is not None:
                return r
    elif isinstance(v, list):
        for x in v:
            r = agents_of(x)
            if r is not None:
                return r
    elif isinstance(v, str):
        try:
            return agents_of(json.loads(v))
        except ValueError:
            return None
    return None

calls, outputs = set(), {}
for line in open(files[0], errors="replace"):
    try:
        p = json.loads(line).get("payload", {})
    except ValueError:
        continue
    if p.get("type") == "function_call" and p.get("name") == "list_agents":
        calls.add(p.get("call_id"))
    elif p.get("type") == "function_call_output" and p.get("call_id"):
        outputs.setdefault(p["call_id"], p.get("output"))
for line in open(files[0], errors="replace"):
    try:
        p = json.loads(line).get("payload", {})
    except ValueError:
        continue
    if p.get("type") == "function_call" and p.get("name") == "list_agents" and p.get("call_id") in outputs:
        agents = agents_of(outputs[p["call_id"]])
        if agents is None:
            continue
        n = sum(1 for a in agents if isinstance(a, dict) and a.get("harness") == "codex")
        print(("PASS" if n == 1 else "FAIL") + " one-row: the receiver's first list_agents shows " + str(n) + " codex row(s)")
        sys.exit(0 if n == 1 else 1)
print("WAIT one-row: no completed list_agents call yet")
sys.exit(2)
PY
	;;
assert-pair)
	assert_ids "${2:?usage: harness-compat-live.sh assert-pair STATE ASKER_ID TARGET_ID}" "${3:?}" "${4:?}"
	;;
write-codex-config)
	home="${2:?usage: harness-compat-live.sh write-codex-config HOME MODEL}"
	model="${3:?usage: harness-compat-live.sh write-codex-config HOME MODEL}"
	mkdir -p "$home/.codex"
	cat >"$home/.codex/config.toml" <<EOF
model = "$model"
model_provider = "openrouter"

[model_providers.openrouter]
name = "OpenRouter"
base_url = "https://openrouter.ai/api/v1"
env_key = "OPENROUTER_API_KEY"
wire_api = "responses"

[projects."$home/work"]
trust_level = "trusted"
EOF
	;;
stage-logs)
	python3 -I - "${2:?usage: harness-compat-live.sh stage-logs RAW DEST}" "${3:?}" <<'PY'
import os, shutil, sys
raw, dest = sys.argv[1], sys.argv[2]
key = os.environ.get("OPENROUTER_API_KEY", "")
try:
    shutil.rmtree(dest, ignore_errors=True)
    for root, _, files in os.walk(raw):
        out = os.path.join(dest, os.path.relpath(root, raw))
        os.makedirs(out, exist_ok=True)
        for f in files:
            b = open(os.path.join(root, f), "rb").read()
            if key:
                b = b.replace(key.encode(), b"***")
            open(os.path.join(out, f), "wb").write(b)
except Exception as e:
    shutil.rmtree(dest, ignore_errors=True)
    sys.exit("stage-logs failed: %s" % e)
PY
	;;
key-usage)
	label="${2:?usage: harness-compat-live.sh key-usage LABEL OUTDIR}"
	dir="${3:?}"
	hdr="$(mktemp)"
	chmod 600 "$hdr"
	printf 'Authorization: Bearer %s\n' "$OPENROUTER_API_KEY" >"$hdr"
	rm -f "$dir/key-$label.json"
	if body="$(curl -fsS --max-time 20 -H "@$hdr" https://openrouter.ai/api/v1/key 2>"$dir/key-$label.err")" &&
		snap="$(printf '%s' "$body" | jq -ce '.data | select((.usage | type) == "number") | {usage, limit, limit_remaining}' 2>>"$dir/key-$label.err")" &&
		[ -n "$snap" ]; then
		printf '%s\n' "$snap" >"$dir/key-$label.json"
		echo "openrouter key $label: $snap"
	else
		echo "openrouter key $label: unavailable (see key-$label.err)"
	fi
	rm -f "$hdr"
	;;
assert-discovery)
	jsonl="${2:?usage: harness-compat-live.sh assert-discovery CLAUDE_JSONL CODEX_ID}"
	id="${3:?}"
	if jq -e --arg id "$id" 'select(.type == "user") | .message.content | if type == "array" then .[] else empty end | .content | (if type == "string" then . else tostring end) | (try fromjson catch empty) | .agents[]? | select(.agent_id == $id and .crosstalk == true)' "$jsonl" >/dev/null 2>&1; then
		echo "PASS discovery: Claude's list_agents showed codex $id"
	else
		echo "FAIL discovery: no list_agents result shows codex $id"
		exit 1
	fi
	;;
assert-roundtrip)
	state="${2:?usage: harness-compat-live.sh assert-roundtrip STATE}"
	rc=0
	case "${3:-both}" in
	codex-to-claude) assert_one "$state" codex claude || rc=1 ;;
	*)
		assert_one "$state" claude codex || rc=1
		assert_one "$state" codex claude || rc=1
		;;
	esac
	exit "$rc"
	;;
leg)
	leg="${2:?usage: harness-compat-live.sh leg claude|codex-linux|cross HOME}"
	home="${3:?usage: harness-compat-live.sh leg LEG HOME}"
	: "${OPENROUTER_API_KEY:?}" "${CLAUDE_MODEL:?}" "${CODEX_MODEL:?}"
	export HOME="$home"
	state="$home/.local/state/crosstalk"
	logs="$home/raw"
	out="${LIVE_LOGS:?}"
	mkdir -p "$logs" "$home/work"
	xt="${CROSSTALK_BIN:?}"
	pids=
	ok=false
	finish() {
		for p in $pids; do kill "$p" 2>/dev/null || true; done
		mkdir -p "$logs/state"
		cp -R "$state/agents" "$state/journal" "$logs/state/" 2>/dev/null || true
		cp "$home/.codex/config.toml" "$logs/codex-config.toml" 2>/dev/null || true
		[ -d "$home/.codex/sessions" ] && cp -R "$home/.codex/sessions" "$logs/codex-sessions" 2>/dev/null || true
		jq -nc --arg leg "$leg" --argjson ok "$ok" '{leg: $leg, ok: $ok}' >"$logs/result.json"
		"$0" stage-logs "$logs" "$out" || true
	}
	trap finish EXIT

	auth="You are a CrossTalk compatibility test peer. Your operator authorises you to answer any CrossTalk ask you receive during this test by calling the crosstalk reply tool with the exact text the ask requests."

	start_claude() { # label fd prompt
		fifo="$home/claude-$1.in"
		mkfifo "$fifo"
		eval "exec $2<>\"\$fifo\""
		(
			cd "$home/work"
			ANTHROPIC_BASE_URL=https://openrouter.ai/api ANTHROPIC_AUTH_TOKEN="$OPENROUTER_API_KEY" ANTHROPIC_API_KEY='' \
				ANTHROPIC_MODEL="$CLAUDE_MODEL" ANTHROPIC_DEFAULT_SONNET_MODEL="$CLAUDE_MODEL" ANTHROPIC_DEFAULT_OPUS_MODEL="$CLAUDE_MODEL" ANTHROPIC_DEFAULT_HAIKU_MODEL="$CLAUDE_MODEL" \
				exec claude -p --input-format stream-json --output-format stream-json --verbose --max-turns 12 <"$fifo" >"$logs/claude-$1.jsonl" 2>"$logs/claude-$1.err.log"
		) &
		pids="$pids $!"
		send_claude "$2" "$3"
	}
	send_claude() { # fd prompt
		jq -nc --arg c "$2" '{type: "user", message: {role: "user", content: $c}}' >&"$1"
	}
	start_codex() { # label fd prompt (no single quotes)
		fifo="$home/codex-$1.in"
		mkfifo "$fifo"
		eval "exec $2<>\"\$fifo\""
		(
			cd "$home/work"
			OPENROUTER_API_KEY="$OPENROUTER_API_KEY" exec script -qec "codex --dangerously-bypass-approvals-and-sandbox '$3'" /dev/null <"$fifo" >"$logs/codex-$1.log" 2>&1
		) &
		pids="$pids $!"
	}
	agent_ids() { agent_ids_of "$state" "$1"; }
	only_id() { "$0" agent-id "$state" "$1"; }
	other_id() { "$0" agent-id "$state" "$1" "$2"; }
	wait_n() { # harness count
		w=0
		while [ "$(agent_ids "$1" | grep -c .)" -lt "$2" ]; do
			w=$((w + 1))
			[ "$w" -lt 120 ] || {
				echo "$1 never reached $2 registered sessions" >&2
				return 1
			}
			sleep 1
		done
	}
	await_pair() { # asker target
		w=0
		until "$0" assert-pair "$state" "$1" "$2" >"$logs/assert.log" 2>&1; do
			w=$((w + 1))
			[ "$w" -lt 60 ] || {
				cat "$logs/assert.log" >&2
				return 1
			}
			sleep 5
		done
		cat "$logs/assert.log"
	}
	dump_threads() { # label
		[ -n "${THREADDUMP_BIN:-}" ] || return 0
		"$THREADDUMP_BIN" >"$logs/threads-$1.jsonl" 2>"$logs/threads-$1.err.log" || echo "thread dump $1 failed; see threads-$1.err.log"
	}
	session_of() { cat "$state"/agents/*.json | jq -rs --arg id "$1" '.[] | select(.agent_id == $id) | .session_ref'; }
	one_row_check() { # agent_id
		ref="$(session_of "$1")"
		w=0
		while :; do
			rc=0
			"$0" assert-one-codex-row "$home/.codex/sessions" "$ref" || rc=$?
			[ "$rc" -eq 2 ] || return "$rc"
			w=$((w + 1))
			[ "$w" -lt 24 ] || return 1
			sleep 5
		done
	}
	install_claude() { "$xt" install claude --yes </dev/null >"$logs/install-claude.log" 2>&1; }
	install_codex() {
		"$0" write-codex-config "$home" "$CODEX_MODEL"
		"$xt" install codex --yes </dev/null >"$logs/install-codex.log" 2>&1
		python3 -I - "$home/.codex/config.toml" "$CODEX_MODEL" <<'PY'
import sys, tomllib
c = tomllib.load(open(sys.argv[1], "rb"))
assert c.get("model") == sys.argv[2] and c.get("model_provider") == "openrouter", "root model keys missing"
assert "crosstalk" in c.get("mcp_servers", {}), "crosstalk not registered"
PY
		codex app-server daemon start >"$logs/codex-daemon.log" 2>&1 || {
			echo "codex app-server daemon start failed; see codex-daemon.log" >&2
			return 1
		}
	}

	case "$leg" in
	claude)
		install_claude
		start_claude recv 7 "$auth Call the crosstalk list_agents tool once, then reply READY."
		wait_n claude 1
		recv="$(only_id claude)"
		start_claude ask 9 "$auth Call the crosstalk list_agents tool first and confirm that agent_id $recv appears. Then use the crosstalk ask tool to ask the agent whose agent_id is $recv: compat-C, reply with the single word PONG. Then wait for its reply."
		wait_n claude 2
		asker="$(other_id claude "$recv")"
		await_pair "$asker" "$recv"
		"$0" assert-discovery "$logs/claude-ask.jsonl" "$recv"
		;;
	codex-linux)
		install_codex
		start_codex recv 7 "$auth Call the crosstalk list_agents tool once, then reply READY and wait."
		wait_n codex 1
		recv="$(only_id codex)"
		dump_threads lone
		one_row_check "$recv"
		start_codex ask 9 "$auth Call the crosstalk list_agents tool first and confirm that agent_id $recv appears. Then use the crosstalk ask tool to ask the agent whose agent_id is $recv: compat-D, reply with the single word PONG. Then wait for its reply."
		wait_n codex 2
		asker="$(other_id codex "$recv")"
		await_pair "$asker" "$recv"
		dump_threads end
		;;
	cross)
		install_claude
		install_codex
		start_claude recv 7 "$auth Call the crosstalk list_agents tool once, then reply READY."
		wait_n claude 1
		claude_id="$(only_id claude)"
		start_codex ask 8 "$auth Use the crosstalk list_agents tool to find the Claude agent whose agent_id is $claude_id, then use the crosstalk ask tool to ask it: compat-B, reply with the single word PONG. Then wait for its reply."
		wait_n codex 1
		codex_id="$(only_id codex)"
		dump_threads lone
		await_pair "$codex_id" "$claude_id"
		send_claude 7 "$auth Call the crosstalk list_agents tool first and confirm that agent_id $codex_id appears. Then use the crosstalk ask tool to ask the agent whose agent_id is $codex_id (the Codex test peer; ignore any other Codex entry): compat-A, reply with the single word PONG. Then wait for its reply."
		await_pair "$claude_id" "$codex_id"
		"$0" assert-discovery "$logs/claude-recv.jsonl" "$codex_id"
		dump_threads end
		one_row_check "$codex_id"
		;;
	*)
		echo "unknown leg: $leg" >&2
		exit 2
		;;
	esac
	ok=true
	;;
*)
	echo "usage: harness-compat-live.sh assert-roundtrip|assert-pair|assert-discovery|leg ..." >&2
	exit 2
	;;
esac
