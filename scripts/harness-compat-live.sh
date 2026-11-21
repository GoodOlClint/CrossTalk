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

wait_agent() {
	state="$1" h="$2" n=0
	while ! jq -e --arg h "$h" 'select(.harness == $h)' "$state"/agents/*.json >/dev/null 2>&1; do
		n=$((n + 1))
		[ "$n" -lt 90 ] || return 1
		sleep 1
	done
}

case "${1:-}" in
assert-roundtrip)
	state="${2:?usage: harness-compat-live.sh assert-roundtrip STATE}"
	rc=0
	assert_one "$state" claude codex || rc=1
	assert_one "$state" codex claude || rc=1
	exit "$rc"
	;;
live-linux)
	home="${2:?usage: harness-compat-live.sh live-linux HOME}"
	: "${OPENROUTER_API_KEY:?}" "${CLAUDE_MODEL:?}" "${CODEX_MODEL:?}"
	export HOME="$home"
	state="$home/.local/state/crosstalk"
	logs="${LIVE_LOGS:-$home/live-logs}"
	mkdir -p "$logs" "$home/work"
	xt="${CROSSTALK_BIN:?}"
	pids=
	finish() {
		for p in $pids; do kill "$p" 2>/dev/null || true; done
		mkdir -p "$logs/state"
		cp -R "$state/agents" "$state/journal" "$logs/state/" 2>/dev/null || true
		cp "$home/.codex/config.toml" "$logs/codex-config.toml" 2>/dev/null || true
		for f in "$logs"/*.log "$logs"/*.jsonl; do
			[ -f "$f" ] && sed -i "s/$OPENROUTER_API_KEY/***/g" "$f"
		done
	}
	trap finish EXIT

	"$xt" install claude --yes </dev/null >"$logs/install-claude.log" 2>&1
	"$xt" install codex --yes </dev/null >"$logs/install-codex.log" 2>&1
	cat >>"$home/.codex/config.toml" <<EOF
model = "$CODEX_MODEL"
model_provider = "openrouter"

[model_providers.openrouter]
name = "OpenRouter"
base_url = "https://openrouter.ai/api/v1"
env_key = "OPENROUTER_API_KEY"
wire_api = "responses"

[projects."$home/work"]
trust_level = "trusted"
EOF
	codex app-server daemon start >"$logs/codex-daemon.log" 2>&1 || {
		echo "codex app-server daemon start failed (open point: Codex daemon on Linux); see codex-daemon.log" >&2
		exit 1
	}

	cin="$home/claude.in"
	mkfifo "$cin"
	exec 7<>"$cin"
	(
		cd "$home/work"
		ANTHROPIC_BASE_URL=https://openrouter.ai/api ANTHROPIC_AUTH_TOKEN="$OPENROUTER_API_KEY" ANTHROPIC_API_KEY= \
			ANTHROPIC_MODEL="$CLAUDE_MODEL" ANTHROPIC_DEFAULT_SONNET_MODEL="$CLAUDE_MODEL" ANTHROPIC_DEFAULT_OPUS_MODEL="$CLAUDE_MODEL" ANTHROPIC_DEFAULT_HAIKU_MODEL="$CLAUDE_MODEL" \
			exec claude -p --input-format stream-json --output-format stream-json --verbose --max-turns 12 <"$cin" >"$logs/claude.jsonl" 2>"$logs/claude.err.log"
	) &
	pids="$pids $!"
	printf '%s\n' '{"type":"user","message":{"role":"user","content":"Call the crosstalk list_agents tool once, then reply READY."}}' >&7
	wait_agent "$state" claude || {
		echo "claude never registered with CrossTalk" >&2
		exit 1
	}

	xin="$home/codex.in"
	mkfifo "$xin"
	exec 8<>"$xin"
	prompt_b="Use the crosstalk list_agents tool to find the Claude agent, then use the crosstalk ask tool to ask it: compat-B, reply with the single word PONG. Then wait for its reply."
	(
		cd "$home/work"
		OPENROUTER_API_KEY="$OPENROUTER_API_KEY" exec script -qec "codex --dangerously-bypass-approvals-and-sandbox '$prompt_b'" /dev/null <"$xin" >"$logs/codex.log" 2>&1
	) &
	pids="$pids $!"
	wait_agent "$state" codex || {
		echo "codex never registered with CrossTalk" >&2
		exit 1
	}

	printf '%s\n' '{"type":"user","message":{"role":"user","content":"Use the crosstalk list_agents tool to find the Codex agent, then use the crosstalk ask tool to ask it: compat-A, reply with the single word PONG. Then wait for its reply."}}' >&7

	n=0
	while :; do
		if "$0" assert-roundtrip "$state" >"$logs/assert.log" 2>&1; then
			cat "$logs/assert.log"
			exit 0
		fi
		n=$((n + 1))
		[ "$n" -lt 60 ] || {
			cat "$logs/assert.log" >&2
			exit 1
		}
		sleep 5
	done
	;;
*)
	echo "usage: harness-compat-live.sh assert-roundtrip|live-linux ..." >&2
	exit 2
	;;
esac
