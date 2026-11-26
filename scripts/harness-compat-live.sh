#!/bin/sh
set -eu

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

ptyrun_path() { # absolute path of ptyrun.py next to this script, whatever the caller's cwd
	d="$(cd "$(dirname "$0")" && pwd)"
	printf '%s/ptyrun.py\n' "$d"
}

case "${1:-}" in
ptyrun-path)
	ptyrun_path
	;;
rollout-ready)
	python3 -I - "${2:?usage: harness-compat-live.sh rollout-ready ROLLOUT_DIR SESSION_REF}" "${3:?}" <<'PY'
import glob, json, sys
root, ref = sys.argv[1], sys.argv[2]
files = [f for f in glob.glob(root + "/**/rollout-*.jsonl", recursive=True) if f.endswith("-" + ref + ".jsonl")]
for f in files:
    for line in open(f, errors="replace"):
        try:
            payload = json.loads(line).get("payload", {})
        except ValueError:
            continue
        if payload.get("type") == "task_complete":
            sys.exit(0)
sys.exit(1)
PY
	;;
assert-home-descriptor)
	state="${2:?usage: harness-compat-live.sh assert-home-descriptor STATE HARNESS}"
	if cat "$state"/agents/*.json 2>/dev/null | jq -es --arg h "${3:?}" 'map(select(.harness == $h)) | length > 0' >/dev/null 2>&1; then
		echo "PASS home: a $3 descriptor exists under the scratch state $state"
	else
		echo "FAIL home: no $3 descriptor under the scratch state $state, so the harness-launched shim did not resolve the leg's HOME"
		exit 1
	fi
	;;
turns)
	python3 -I - "${2:?usage: harness-compat-live.sh turns ROLLOUT_DIR SESSION_REF}" "${3:?}" <<'PY'
import glob, json, sys
root, ref = sys.argv[1], sys.argv[2]
n = {"task_started": 0, "task_complete": 0, "turn_aborted": 0}
for f in glob.glob(root + "/**/rollout-*.jsonl", recursive=True):
    if not f.endswith("-" + ref + ".jsonl"):
        continue
    for line in open(f, errors="replace"):
        try:
            kind = json.loads(line).get("payload", {}).get("type")
        except ValueError:
            continue
        if kind in n:
            n[kind] += 1
print(n["task_started"], n["task_complete"], n["turn_aborted"])
PY
	;;
events)
	python3 -I - "${2:?usage: harness-compat-live.sh events ROLLOUT_DIR SESSION_REF}" "${3:?}" <<'PY'
import glob, json, sys
root, ref = sys.argv[1], sys.argv[2]
for f in glob.glob(root + "/**/rollout-*.jsonl", recursive=True):
    if not f.endswith("-" + ref + ".jsonl"):
        continue
    for line in open(f, errors="replace"):
        try:
            p = json.loads(line).get("payload", {})
        except ValueError:
            continue
        kind = p.get("type")
        if kind == "task_started":
            print("queue" if (p.get("turn_attribution") or {}).get("turn_trigger") == "queue" else "user")
        elif kind == "task_complete":
            print("done")
        elif kind == "turn_aborted":
            print("abort")
PY
	;;
wake-state)
	jq -r '.state // "missing"' "${2:?usage: harness-compat-live.sh wake-state STATE SESSION_REF}/codex-wakes/${3:?}.json" 2>/dev/null || echo missing
	;;
assert-wake)
	state="${2:?usage: harness-compat-live.sh assert-wake STATE SESSION_REF}"
	ref="${3:?}"
	got="$(jq -r '.state // empty' "$state/codex-wakes/$ref.json" 2>/dev/null || true)"
	if [ "$got" = collected ]; then
		echo "PASS wake: CrossTalk's wake generation for $ref was queued and closed by a pull (collected)"
	else
		echo "FAIL wake: generation state for $ref is '${got:-missing}', want collected"
		exit 1
	fi
	;;
agent-id)
	ids="$(agent_ids_of "${2:?usage: harness-compat-live.sh agent-id STATE HARNESS [EXCLUDE]}" "${3:?}" "${4-}")"
	[ "$(printf '%s\n' "$ids" | grep -c .)" -eq 1 ] || {
		echo "expected exactly one ${3} descriptor${4:+ besides $4}, got: $ids" >&2
		exit 1
	}
	printf '%s\n' "$ids"
	;;
check-staged)
	d="${2:?usage: harness-compat-live.sh check-staged STAGED_STATE_DIR SESSION_REF}"
	ref="${3:?}"
	[ -f "$d/codex-wakes/$ref.json" ] || {
		echo "staged artifact lacks the wake generation record for $ref" >&2
		exit 1
	}
	ls "$d"/codex-claims/*-"$ref".json >/dev/null 2>&1 || {
		echo "staged artifact lacks the claim record for $ref" >&2
		exit 1
	}
	echo "PASS staged: the artifact carries the wake generation record codex-wakes/$ref.json and the claim record codex-claims/*-$ref.json"
	;;
assert-own-delivery-mode)
	python3 -I - "${2:?usage: harness-compat-live.sh assert-own-delivery-mode ROLLOUT_DIR SESSION_REF AGENT_ID}" "${3:?}" "${4:?}" <<'PY'
import glob, json, sys
root, ref, agent = sys.argv[1], sys.argv[2], sys.argv[3]
files = [f for f in glob.glob(root + "/**/rollout-*.jsonl", recursive=True) if f.endswith("-" + ref + ".jsonl")]

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

for f in files:
    for line in open(f, errors="replace"):
        try:
            item = json.loads(line).get("payload", {}).get("item") or {}
        except ValueError:
            continue
        if item.get("type") == "McpToolCall" and item.get("server") == "crosstalk" and item.get("tool") == "list_agents" and item.get("status") == "completed":
            for a in agents_of(item.get("result")) or []:
                if isinstance(a, dict) and a.get("agent_id") == agent and str(a.get("delivery_mode", "")).startswith("queue-wake"):
                    print("PASS delivery mode: the tab's own list_agents shows " + agent + " as queue-wake")
                    sys.exit(0)
print("FAIL delivery mode: no list_agents result in the tab's rollout shows " + agent + " as queue-wake")
sys.exit(1)
PY
	;;
assert-mailbox)
	state="${2:?usage: harness-compat-live.sh assert-mailbox STATE SENDER_ID RECEIVER_ID}"
	sender="${3:?}"
	receiver="${4:?}"
	ids="$(jq -r --arg to "$receiver" 'select(.type == "outbound" and .kind == "inform" and .to == $to) | .message_id' "$state/journal/$sender.jsonl" 2>/dev/null | sort -u)"
	[ -n "$ids" ] || {
		echo "FAIL mailbox: no inform sent to $receiver"
		exit 1
	}
	for m in $ids; do
		got="$(jq -r --arg m "$m" 'select(.message_id == $m and (.type == "dedupe" or .type == "outcome")) | .state' "$state/journal/$receiver.jsonl" 2>/dev/null | sort -u | tr '\n' ' ')"
		case " $got" in *" mailboxed "*)
			case " $got" in *" delivered "* | *" confirmed "*)
				echo "PASS mailbox: inform $m went mailboxed then pulled (states: $got)"
				exit 0
				;;
			esac
			;;
		esac
	done
	echo "FAIL mailbox: no inform reached mailboxed and delivered (last states: ${got:-none})"
	exit 1
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

for line in open(files[0], errors="replace"):
    try:
        item = json.loads(line).get("payload", {}).get("item") or {}
    except ValueError:
        continue
    if item.get("type") == "McpToolCall" and item.get("server") == "crosstalk" and item.get("tool") == "list_agents":
        if item.get("status") != "completed":
            print("FAIL one-row: the first list_agents call ended " + str(item.get("status")))
            sys.exit(1)
        agents = agents_of(item.get("result"))
        if agents is None:
            print("FAIL one-row: the first list_agents output could not be read")
            sys.exit(1)
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
import base64
secrets = [os.environ.get("OPENROUTER_API_KEY", "")]
pw = os.environ.get("OPENCODE_SERVICE_PASSWORD", "")
if pw:
    secrets += [pw, base64.b64encode(("opencode:" + pw).encode()).decode()]
try:
    shutil.rmtree(dest, ignore_errors=True)
    for root, _, files in os.walk(raw):
        out = os.path.join(dest, os.path.relpath(root, raw))
        os.makedirs(out, exist_ok=True)
        for f in files:
            b = open(os.path.join(root, f), "rb").read()
            for secret in secrets:
                if secret:
                    b = b.replace(secret.encode(), b"***")
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
	if jq -e --arg id "$id" 'select(.type == "user") | .message.content | if type == "array" then .[] | objects else empty end | .content | (if type == "string" then . else tostring end) | (try fromjson catch empty) | objects | .agents[]? | select(.agent_id == $id and .crosstalk == true)' "$jsonl" >/dev/null 2>&1; then
		echo "PASS discovery: Claude's list_agents showed codex $id"
	else
		echo "FAIL discovery: no list_agents result shows codex $id"
		exit 1
	fi
	;;
assert-pi-no-turn)
	usage="usage: harness-compat-live.sh assert-pi-no-turn STATE RECEIVER_ID SESSIONS_DIR SESSION_ID MARKER"
	state="${2:?$usage}"
	recv="${3:?$usage}"
	dir="${4:?$usage}"
	id="${5:?$usage}"
	marker="${6:?$usage}"
	jq -es --arg m "$marker" '([.[] | select(any(.message.parts[]?; .text | contains($m))) | .message_id]) as $ids | any(.[]; .state == "queued" and (.message_id as $i | $ids | index($i)))' "$state/journal/$recv.jsonl" >/dev/null 2>&1 || {
		echo "FAIL no-turn: no inbound message carrying $marker is queued at $recv"
		exit 1
	}
	hp="$(jq -r .harness_pid "$state/agents/$recv.json")"
	cat "$state/sock/pi/$hp".*.status 2>/dev/null | jq -es 'length > 0 and all(.[]; .status == "idle")' >/dev/null || {
		echo "FAIL no-turn: the receiver is not idle after the delivery, so a run started or its state is unknown"
		exit 1
	}
	f="$(find "$dir" -type f -name "*_$id.jsonl" 2>/dev/null)"
	[ -n "$f" ] || {
		echo "FAIL no-turn: no session file for $id"
		exit 1
	}
	jq -es --arg m "$marker" '(map(.type == "custom_message" and (.content | tostring | contains($m))) | index(true)) as $i | $i != null and all(.[$i + 1:][]; .message.role? != "assistant")' "$f" >/dev/null || {
		echo "FAIL no-turn: the text is not shown in the session, or a turn followed it"
		exit 1
	}
	echo "PASS no-turn: the next_turn text is shown in the receiver's session, queued, with no turn after it"
	;;
opencode-service)
	f="${2:?usage: harness-compat-live.sh opencode-service SERVICE_JSON [LAUNCHED_PID]}"
	launched="${3-}"
	command -v lsof >/dev/null 2>&1 || {
		echo "lsof not found: cannot check which process owns the service port" >&2
		exit 1
	}
	[ -f "$f" ] || {
		echo "no $f yet" >&2
		exit 1
	}
	url="$(jq -r '.url // ""' "$f")" || exit 1
	printf '%s\n' "$url" | grep -Eq '^http://(127\.0\.0\.1|\[::1\]):[0-9]+/?$' || {
		echo "service.json url is not a loopback http origin" >&2
		exit 1
	}
	port="$(printf '%s\n' "$url" | sed -E 's|^.*:([0-9]+)/?$|\1|')"
	spid="$(jq -r '.pid | if type == "number" then tostring else "" end' "$f")"
	listener="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true)"
	[ -n "$spid" ] && [ "$spid" = "$listener" ] || {
		echo "service.json pid '$spid' is not the listener of port $port ('$listener')" >&2
		exit 1
	}
	[ -z "$launched" ] || [ "$spid" = "$launched" ] || {
		echo "service.json pid '$spid' is not the service this leg launched ('$launched')" >&2
		exit 1
	}
	printf '%s\n' "$url"
	;;
leg)
	leg="${2:?usage: harness-compat-live.sh leg claude-linux|claude-macos|codex-linux|codex-macos|opencode-linux|opencode-macos|pi-linux|pi-macos HOME}"
	home="${3:?usage: harness-compat-live.sh leg LEG HOME}"
	: "${OPENROUTER_API_KEY:?}"
	case "$leg" in
	claude-*) : "${CLAUDE_MODEL:?}" ;;
	opencode-*) : "${OPENCODE_MODEL:?}" ;;
	pi-*) : "${PI_COMPAT_MODEL:?}" ;;
	*) : "${CODEX_MODEL:?}" ;;
	esac
	if [ "$leg" = codex-macos ]; then
		# The queue phase of this leg runs Codex with hook trust bypassed: refuse before creating, copying or installing anything unless the home is a fresh, empty directory under /tmp.
		real="$(cd "$home" 2>/dev/null && pwd -P)" || real=
		case "$real" in
		"$(cd /tmp && pwd -P)"/*) ;;
		*)
			echo "codex-macos needs a scratch directory under /tmp as its home, not '$home'" >&2
			exit 1
			;;
		esac
		[ -z "$(ls -A "$real")" ] || {
			echo "codex-macos needs an empty scratch home, '$home' has content" >&2
			exit 1
		}
	fi
	case "$leg" in
	pi-*)
		# Before the log trap: a refused home must not have its transcripts staged as artifacts.
		[ -d "$home" ] && [ -z "$(ls -A "$home")" ] || {
			echo "pi-* needs an empty scratch home, '$home' is not one" >&2
			exit 1
		}
		;;
	esac
	export HOME="$home"
	state="$home/.local/state/crosstalk"
	logs="$home/raw"
	out="${LIVE_LOGS:?}"
	mkdir -p "$logs" "$home/work"
	xt="${CROSSTALK_BIN:?}"
	PTYRUN="${PTYRUN:-$(ptyrun_path)}"
	CODEX_HOME="$home/.codex"
	export CODEX_HOME
	case "$leg" in
	opencode-*)
		export XDG_CONFIG_HOME="$home/.config" XDG_STATE_HOME="$home/.local/state" XDG_DATA_HOME="$home/.local/share" XDG_CACHE_HOME="$home/.cache"
		export OPENCODE_DISABLE_AUTOUPDATE=1 OPENCODE_DISABLE_MODELS_FETCH=1 OPENCODE_DISABLE_CLAUDE_CODE=1
		;;
	pi-*)
		export PI_CODING_AGENT_DIR="$home/.pi/agent" PI_TELEMETRY=0
		unset PI_CODING_AGENT_SESSION_DIR
		;;
	esac
	pids=
	ok=false
	tab_ref=
	finish() {
		for p in $pids; do kill "$p" 2>/dev/null || true; done
		mkdir -p "$logs/state"
		cp -R "$state/agents" "$state/journal" "$state/codex-wakes" "$state/codex-claims" "$logs/state/" 2>/dev/null || true
		cp "$home/.codex/config.toml" "$logs/codex-config.toml" 2>/dev/null || true
		if [ -f "$home/.local/state/opencode/service.json" ]; then jq '.password = "***"' "$home/.local/state/opencode/service.json" >"$logs/opencode-service.json" 2>/dev/null || true; fi
		cp "$home/.config/opencode/opencode.json" "$logs/opencode-config.json" 2>/dev/null || true
		[ -d "$home/.codex/sessions" ] && cp -R "$home/.codex/sessions" "$logs/codex-sessions" 2>/dev/null || true
		[ -d "$home/.pi/agent/sessions" ] && cp -R "$home/.pi/agent/sessions" "$logs/pi-sessions" 2>/dev/null || true
		staged=0
		"$0" stage-logs "$logs" "$out" || staged=1
		if [ "$ok" = true ] && [ "$leg" = codex-macos ] && [ "$staged" -eq 0 ]; then
			"$0" check-staged "$out/state" "$tab_ref" || staged=1
		fi
		[ "$staged" -eq 0 ] || ok=false
		jq -nc --arg leg "$leg" --argjson ok "$ok" '{leg: $leg, ok: $ok}' >"$logs/result.json"
		"$0" stage-logs "$logs" "$out" || true
		[ "$staged" -eq 0 ] || exit 1
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
	start_codex() { # label fd prompt (no single quotes): a daemon-mode tab in a pty
		fifo="$home/codex-$1.in"
		mkfifo "$fifo"
		eval "exec $2<>\"\$fifo\""
		(
			cd "$home/work"
			exec python3 "${PTYRUN:?}" "$logs/codex-$1.log" "$fifo" codex --dangerously-bypass-approvals-and-sandbox "$3"
		) &
		pids="$pids $!"
	}
	start_codex_nd() { # label fd prompt: a --no-daemon tab in a pty
		fifo="$home/codex-$1.in"
		mkfifo "$fifo"
		eval "exec $2<>\"\$fifo\""
		(
			cd "$home/work"
			exec env PTYRUN_ANSWER_HOOKS=1 python3 "${PTYRUN:?}" "$logs/codex-$1.log" "$fifo" codex --no-daemon --dangerously-bypass-hook-trust --dangerously-bypass-approvals-and-sandbox "$3"
		) &
		pids="$pids $!"
	}
	await_ready() { # session_ref: the tab's first turn has finished (a task_complete event is in its rollout; model prose is never matched)
		w=0
		until "$0" rollout-ready "$home/.codex/sessions" "$1" >/dev/null 2>&1; do
			w=$((w + 1))
			[ "$w" -lt 36 ] || {
				echo "timed out after 180 s waiting for a task_complete event in the rollout of thread $1 (its first turn never finished)" >&2
				return 1
			}
			sleep 5
		done
	}
	assert_home_resolved() { "$0" assert-home-descriptor "$state" "$1"; }
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
	type_line() { # fd text: the TUI treats an Enter that arrives with the text as a pasted newline, so Enter is its own write
		printf '%s' "$2" >&"$1"
		sleep 1
		printf '\r' >&"$1"
	}
	interrupted_case() { # tab_ref codex_id: plan 3 b probe 3, fds 8 (tab) and 9 (sender) are the queue phase's pty tabs
		iref="$1" icid="$2"
		fail() {
			echo "FAIL interrupted: $1"
			echo "tab rollout events (last 12):"
			find "$home/.codex/sessions" -name "rollout-*-$iref.jsonl" -exec grep -ho '"type":"[A-Za-z_]*"' {} + 2>/dev/null | tail -n 12 | paste -sd' ' -
			return 1
		}
		w=0
		while :; do
			read -r s c _ <<EOF
$("$0" turns "$home/.codex/sessions" "$iref")
EOF
			[ "$s" -eq "$c" ] && break
			w=$((w + 1))
			[ "$w" -lt 45 ] || fail "the tab's earlier turn never finished (started $s, completed $c)" || return 1
			sleep 2
		done
		read -r s0 c0 _ <<EOF
$("$0" turns "$home/.codex/sessions" "$iref")
EOF
		type_line 8 "Run the shell command: sleep 120. Then say DONE."
		w=0
		while :; do
			read -r s c _ <<EOF
$("$0" turns "$home/.codex/sessions" "$iref")
EOF
			[ "$s" -gt "$s0" ] && [ "$c" -eq "$c0" ] && break
			w=$((w + 1))
			[ "$w" -lt 60 ] || fail "the tab never started a long turn (started $s, completed $c)" || return 1
			sleep 2
		done
		type_line 9 "Use the crosstalk send_message tool to send an inform to the agent whose agent_id is $icid with the text: compat-W2, please fetch your mail. Then say SENT."
		w=0
		until [ "$("$0" wake-state "$state" "$iref")" = queued ]; do
			w=$((w + 1))
			[ "$w" -lt 45 ] || fail "no queued wake generation appeared while the tab's turn was running" || return 1
			sleep 2
		done
		wake_id="$(jq -r '.wake_id // empty' "$state/codex-wakes/$iref.json")"
		[ -n "$wake_id" ] || fail "the queued generation records no wake_id" || return 1
		echo "PASS interrupted: a long turn was running and a wake row was queued behind it"
		printf '\033' >&8
		w=0
		while :; do
			read -r s c a <<EOF
$("$0" turns "$home/.codex/sessions" "$iref")
EOF
			[ "$a" -gt 0 ] && break
			w=$((w + 1))
			[ "$w" -lt 15 ] || fail "Esc did not abort the tab's turn (aborted $a)" || return 1
			sleep 2
		done
		n=0
		while [ "$n" -lt 9 ]; do
			sleep 5
			n=$((n + 1))
			read -r s2 _ _ <<EOF
$("$0" turns "$home/.codex/sessions" "$iref")
EOF
			[ "$s2" -eq "$s" ] || fail "a turn started while the thread was interrupted (started $s, now $s2)" || return 1
			[ "$("$0" wake-state "$state" "$iref")" = queued ] || fail "the wake generation left queued while the thread was interrupted" || return 1
			rows="$(sqlite3 -readonly "$home/.codex/queue_1.sqlite" "select count(*) from queued_items where json_extract(payload_json, '\$.UserInput.client_id') = '$wake_id'" 2>&1)" || fail "cannot read the queue: $rows" || return 1
			[ "$rows" -ge 1 ] || fail "the wake row $wake_id was consumed while the thread was interrupted" || return 1
		done
		echo "PASS interrupted: after Esc the turn aborted, and for 45 s no turn started, the wake generation stayed queued and the queue row stayed"
		type_line 8 "Say one word."
		w=0
		while :; do
			read -r s3 _ _ <<EOF
$("$0" turns "$home/.codex/sessions" "$iref")
EOF
			if [ "$s3" -ge $((s + 2)) ] && [ "$("$0" wake-state "$state" "$iref")" = collected ]; then
				order="$("$0" events "$home/.codex/sessions" "$iref" | awk '/^abort$/ { n = 0; next } { n++; a[n] = $0 } END { print a[1], a[2], a[3] }')"
				[ "$order" = "user done queue" ] || fail "the wake did not dispatch after the operator's turn finished (events since the last abort: $order)" || return 1
				break
			fi
			w=$((w + 1))
			[ "$w" -lt 60 ] || fail "after the next prompt the wake did not dispatch (started $s3, generation $("$0" wake-state "$state" "$iref"))" || return 1
			sleep 3
		done
		echo "PASS interrupted: the next prompt ran, then the wake dispatched (generation collected)"
	}
	install_claude() { "$xt" install claude --yes </dev/null >"$logs/install-claude.log" 2>&1; }
	install_codex() { # [queue]
		"$0" write-codex-config "$home" "$CODEX_MODEL"
		if [ "${1:-}" = queue ]; then
			"$xt" install codex --codex-no-daemon-queue --yes </dev/null >"$logs/install-codex.log" 2>&1
		else
			"$xt" install codex --yes </dev/null >"$logs/install-codex.log" 2>&1
		fi
		python3 -I - "$home/.codex/config.toml" "$CODEX_MODEL" <<'PY'
import sys, tomllib
c = tomllib.load(open(sys.argv[1], "rb"))
assert c.get("model") == sys.argv[2] and c.get("model_provider") == "openrouter", "root model keys missing"
assert "crosstalk" in c.get("mcp_servers", {}), "crosstalk not registered"
PY
		[ "${1:-}" != queue ] || return 0
		codex app-server daemon start >"$logs/codex-daemon.log" 2>&1 || {
			echo "codex app-server daemon start failed; see codex-daemon.log" >&2
			return 1
		}
	}

	oc_api() { # method path [body]: the Basic header goes through curl --config - so it is never on a command line
		oc_url="$(cat "$logs/opencode-service.url")"
		if [ $# -ge 3 ]; then
			printf 'user = "opencode:%s"\n' "$OPENCODE_SERVICE_PASSWORD" | curl -fsS --max-time 30 --config - -X "$1" -H 'content-type: application/json' --data "$3" "$oc_url$2"
		else
			printf 'user = "opencode:%s"\n' "$OPENCODE_SERVICE_PASSWORD" | curl -fsS --max-time 30 --config - -X "$1" "$oc_url$2"
		fi
	}
	oc_new_session() { # title
		oc_api POST /api/session "$(jq -nc --arg t "$1" '{title: $t}')" | jq -r '.id // .data.id // empty'
	}
	oc_prompt() { # session_id text: a steer prompt, which wakes an idle session
		oc_api POST "/api/session/$1/prompt" "$(jq -nc --arg t "$2" '{text: $t, delivery: "steer"}')" >"$logs/oc-prompt.json"
	}
	install_opencode() {
		[ ! -e "$XDG_CONFIG_HOME/opencode/opencode.json" ] || {
			echo "opencode-* needs a scratch home: $XDG_CONFIG_HOME/opencode/opencode.json already exists and is not overwritten" >&2
			return 1
		}
		mkdir -p "$XDG_CONFIG_HOME/opencode"
		jq -n --arg m "$OPENCODE_MODEL" '{model: $m}' >"$XDG_CONFIG_HOME/opencode/opencode.json"
		"$xt" install opencode --yes </dev/null >"$logs/install-opencode.log" 2>&1
		jq -e --arg m "$OPENCODE_MODEL" '.model == $m and (.mcp.servers.crosstalk | type == "object")' "$XDG_CONFIG_HOME/opencode/opencode.json" >/dev/null || {
			echo "opencode.json lacks the model or the crosstalk entry after install" >&2
			return 1
		}
	}
	start_opencode_service() {
		command -v lsof >/dev/null 2>&1 || {
			echo "lsof not found: cannot check which process owns the service port" >&2
			return 1
		}
		held="$(lsof -nP -iTCP:"${COMPAT_OPENCODE_PORT:-49374}" -sTCP:LISTEN -t 2>/dev/null || true)"
		[ -z "$held" ] || {
			echo "port ${COMPAT_OPENCODE_PORT:-49374} is already held by pid $held, not by this leg" >&2
			return 1
		}
		(
			cd "$home/work"
			exec opencode serve --service ${COMPAT_OPENCODE_PORT:+--port "$COMPAT_OPENCODE_PORT"}
		) >"$logs/opencode-service.log" 2>&1 &
		launched=$!
		pids="$pids $launched"
		w=0
		until "$0" opencode-service "$XDG_STATE_HOME/opencode/service.json" "$launched" >"$logs/opencode-service.url" 2>"$logs/opencode-service.err"; do
			w=$((w + 1))
			[ "$w" -lt 60 ] || {
				cat "$logs/opencode-service.err" >&2
				return 1
			}
			sleep 1
		done
		OPENCODE_SERVICE_PASSWORD="$(jq -r .password "$XDG_STATE_HOME/opencode/service.json")"
		export OPENCODE_SERVICE_PASSWORD
	}
	opencode_roundtrip() {
		install_opencode
		start_opencode_service
		recv_session="$(oc_new_session recv)"
		[ -n "$recv_session" ] || {
			echo "POST /api/session returned no session id" >&2
			return 1
		}
		oc_prompt "$recv_session" "$auth Call the crosstalk list_agents tool once, then reply READY."
		wait_n opencode 1
		assert_home_resolved opencode
		recv="$(only_id opencode)"
		ask_session="$(oc_new_session ask)"
		[ -n "$ask_session" ] || {
			echo "POST /api/session returned no session id" >&2
			return 1
		}
		oc_prompt "$ask_session" "$auth Call the crosstalk list_agents tool first and confirm that agent_id $recv appears. Then use the crosstalk ask tool to ask the agent whose agent_id is $recv: compat-E, reply with the single word PONG. Then wait for its reply."
		wait_n opencode 2
		asker="$(other_id opencode "$recv")"
		await_pair "$asker" "$recv"
	}

	install_pi() {
		"$xt" install pi --yes </dev/null >"$logs/install-pi.log" 2>&1
	}
	start_pi() { # label fd prompt: an interactive session in a pty
		fifo="$home/pi-$1.in"
		mkfifo "$fifo"
		eval "exec $2<>\"\$fifo\""
		(
			cd "$home/work"
			exec python3 "${PTYRUN:?}" "$logs/pi-$1.log" "$fifo" pi --model "$PI_COMPAT_MODEL" "$3"
		) &
		pids="$pids $!"
	}
	await_idle() { # session_id: its last message is a finished assistant turn
		w=0
		until f="$(find "$home/.pi/agent/sessions" -type f -name "*_$1.jsonl" 2>/dev/null)" && [ -n "$f" ] &&
			jq -es '[.[] | select(.type == "message")] | last | .message | .role == "assistant" and .stopReason == "stop"' "$f" >/dev/null 2>&1; do
			w=$((w + 1))
			[ "$w" -lt 60 ] || {
				echo "the Pi session $1 never finished its turn" >&2
				return 1
			}
			sleep 2
		done
	}
	pi_roundtrip() {
		install_pi
		start_pi recv 7 "$auth Call the crosstalk list_agents tool once, then reply READY."
		wait_n pi 1
		assert_home_resolved pi
		recv="$(only_id pi)"
		start_pi ask 9 "$auth Call the crosstalk list_agents tool first and confirm that agent_id $recv appears. Then use the crosstalk ask tool to ask the agent whose agent_id is $recv: compat-F, reply with the single word PONG. Then wait for its reply."
		wait_n pi 2
		asker="$(other_id pi "$recv")"
		await_pair "$asker" "$recv"
		# A verified local sender's next_turn is shown to an idle session and starts no turn.
		ref="$(session_of "$recv")"
		await_idle "${ref%@*}"
		ask_ref="$(session_of "$asker")"
		await_idle "${ask_ref%@*}"
		type_line 9 "Use the crosstalk send_message tool with delivery next_turn to send the agent whose agent_id is $recv the text: compat-NT. Then say SENT."
		w=0
		until "$0" assert-pi-no-turn "$state" "$recv" "$home/.pi/agent/sessions" "${ref%@*}" compat-NT >"$logs/assert-no-turn.log" 2>&1; do
			w=$((w + 1))
			[ "$w" -lt 45 ] || {
				cat "$logs/assert-no-turn.log" >&2
				return 1
			}
			sleep 2
		done
		sleep 15
		"$0" assert-pi-no-turn "$state" "$recv" "$home/.pi/agent/sessions" "${ref%@*}" compat-NT
	}

	codex_roundtrip() {
		install_codex
		start_codex recv 7 "$auth Call the crosstalk list_agents tool once, then reply READY and wait."
		wait_n codex 1
		assert_home_resolved codex
		recv="$(only_id codex)"
		dump_threads lone
		one_row_check "$recv"
		start_codex ask 9 "$auth Call the crosstalk list_agents tool first and confirm that agent_id $recv appears. Then use the crosstalk ask tool to ask the agent whose agent_id is $recv: compat-D, reply with the single word PONG. Then wait for its reply."
		wait_n codex 2
		asker="$(other_id codex "$recv")"
		await_pair "$asker" "$recv"
		dump_threads end
	}

	case "$leg" in
	claude-linux|claude-macos)
		install_claude
		start_claude recv 7 "$auth Call the crosstalk list_agents tool once, then reply READY."
		wait_n claude 1
		assert_home_resolved claude
		recv="$(only_id claude)"
		start_claude ask 9 "$auth Call the crosstalk list_agents tool first and confirm that agent_id $recv appears. Then use the crosstalk ask tool to ask the agent whose agent_id is $recv: compat-C, reply with the single word PONG. Then wait for its reply."
		wait_n claude 2
		asker="$(other_id claude "$recv")"
		await_pair "$asker" "$recv"
		"$0" assert-discovery "$logs/claude-ask.jsonl" "$recv"
		;;
	codex-linux)
		codex_roundtrip
		;;
	opencode-linux | opencode-macos)
		opencode_roundtrip
		;;
	pi-linux | pi-macos)
		pi_roundtrip
		;;
	codex-macos)
		codex_roundtrip
		for p in $pids; do kill "$p" 2>/dev/null || true; done
		pids=
		codex app-server daemon stop >"$logs/codex-daemon-stop.log" 2>&1 || true
		mkdir -p "$logs/state-shared"
		cp -R "$state/agents" "$state/journal" "$logs/state-shared/" 2>/dev/null || true
		home="$(mktemp -d /tmp/ct.XXXXXX)"
		export HOME="$home"
		state="$home/.local/state/crosstalk"
		CODEX_HOME="$home/.codex"
		export CODEX_HOME
		mkdir -p "$home/work"
		install_codex queue
		start_codex_nd tab 8 "$auth Call the crosstalk list_agents tool once, then finish your turn with one short sentence."
		wait_n codex 1
		assert_home_resolved codex
		codex_id="$(only_id codex)"
		tab_ref="$(session_of "$codex_id")"
		await_ready "$tab_ref"
		start_codex_nd send 9 "$auth Call the crosstalk list_agents tool and confirm that agent_id $codex_id appears. Then use the crosstalk send_message tool to send an inform to the agent whose agent_id is $codex_id with the text: compat-W1, please fetch your mail. Then say SENT."
		wait_n codex 2
		sender_id="$(other_id codex "$codex_id")"
		w=0
		until "$0" assert-mailbox "$state" "$sender_id" "$codex_id" >"$logs/assert-mailbox.log" 2>&1; do
			w=$((w + 1))
			[ "$w" -lt 30 ] || {
				cat "$logs/assert-mailbox.log" >&2
				exit 1
			}
			sleep 5
		done
		cat "$logs/assert-mailbox.log"
		if ! "$0" assert-own-delivery-mode "$home/.codex/sessions" "$tab_ref" "$codex_id" >"$logs/assert-mode.log" 2>&1; then
			# The tab's startup listing can precede the claim being accepted; ask for a fresh one by typing into the tab.
			type_line 8 "Call the crosstalk list_agents tool once more."
			w=0
			until "$0" assert-own-delivery-mode "$home/.codex/sessions" "$tab_ref" "$codex_id" >"$logs/assert-mode.log" 2>&1; do
				w=$((w + 1))
				[ "$w" -lt 24 ] || {
					cat "$logs/assert-mode.log" >&2
					exit 1
				}
				sleep 5
			done
		fi
		cat "$logs/assert-mode.log"
		"$0" assert-wake "$state" "$tab_ref"
		interrupted_case "$tab_ref" "$codex_id"
		;;
	*)
		echo "unknown leg: $leg" >&2
		exit 2
		;;
	esac
	ok=true
	;;
*)
	echo "usage: harness-compat-live.sh assert-pair|assert-discovery|assert-home-descriptor|assert-pi-no-turn|opencode-service|leg ..." >&2
	exit 2
	;;
esac
