#!/bin/sh
set -eu

pkg() {
	case "$1" in
	codex) echo @openai/codex ;;
	claude-code) echo @anthropic-ai/claude-code ;;
	opencode) echo @opencode/cli ;;
	pi) echo @earendil-works/pi-coding-agent ;;
	*)
		echo "unknown harness: $1" >&2
		return 1
		;;
	esac
}

semver() {
	printf '%s\n' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'
}

case "${1:-}" in
check-updates)
	usage="usage: harness-compat.sh check-updates RECORD HARNESS VERSION FORCE RECORDED"
	record="${2:?$usage}"
	which="${3:?$usage}"
	forced="${4-}"
	force="${5-}"
	recorded="${6-}"
	case "$which" in all) set -- codex claude-code opencode pi ;; *) set -- "$which" ;; esac
	if [ -n "$forced" ]; then
		semver "$forced" || {
			echo "version must be x.y.z: $forced" >&2
			exit 1
		}
		[ "$#" -eq 1 ] || {
			echo "a forced version needs one harness, not all" >&2
			exit 1
		}
	fi
	out="[]"
	for h in "$@"; do
		p="$(pkg "$h")"
		latest="${forced:-$(npm view "$p" version)}"
		semver "$latest" || {
			echo "npm returned a non-version for $h: $latest" >&2
			exit 1
		}
		tested="${recorded:-$(jq -r --arg h "$h" '.[$h].tested // "unverified"' "$record")}"
		if [ "$latest" != "$tested" ] || [ "$force" = true ]; then
			out="$(printf '%s' "$out" | jq -c --arg h "$h" --arg v "$latest" '. + [{harness: $h, version: $v}]')"
		fi
	done
	printf '%s\n' "$out"
	;;
legs)
	usage="usage: harness-compat.sh legs CHANGED_JSON [QUEUE_CODEX_VERSION]"
	printf '%s' "${2:?$usage}" | jq -c --arg q "${3-}" '[.[] | (if .harness == "claude-code" then ({leg: "claude-linux", harness: .harness, version: .version, runner: "ubuntu-latest"}, {leg: "claude-macos", harness: .harness, version: .version, runner: "macos-latest"}) elif .harness == "codex" then ({leg: "codex-linux", harness: .harness, version: .version, runner: "ubuntu-latest"}, {leg: "codex-macos", harness: .harness, version: (if $q != "" then $q else .version end), runner: "macos-latest"}) elif .harness == "opencode" then ({leg: "opencode-linux", harness: .harness, version: .version, runner: "ubuntu-latest"}, {leg: "opencode-macos", harness: .harness, version: .version, runner: "macos-latest"}) elif .harness == "pi" then ({leg: "pi-linux", harness: .harness, version: .version, runner: "ubuntu-latest"}, {leg: "pi-macos", harness: .harness, version: .version, runner: "macos-latest"}) else empty end)]'
	;;
step-status)
	usage="usage: harness-compat.sh step-status JOBS_FILE JOB STEP"
	jq -rs --arg j "${3:?$usage}" --arg s "${4:?$usage}" '
		map(select(.name == $j)) | if length == 0 then "fail" else
		.[0] as $job | ($job.steps // [] | map(select(.name == $s)) | .[0].conclusion // "skipped") as $c |
		if $job.conclusion == "success" and $c == "success" then "pass"
		elif $c == "skipped" and $job.conclusion != "failure" then "skipped"
		else "fail" end end' "${2:?$usage}"
	;;
report)
	usage="usage: harness-compat.sh report JOBS_FILE LEGS_JSON"
	jobs="${2:?$usage}"
	legs="${3:?$usage}"
	res="{}"
	for leg in $(printf '%s' "$legs" | jq -r '.[].leg'); do
		st="$("$0" step-status "$jobs" "harness-$leg" "Run the leg")"
		res="$(printf '%s' "$res" | jq -c --arg l "$leg" --arg s "$st" '. + {($l): $s}')"
	done
	printf '%s' "$legs" | jq -c --argjson res "$res" '
		(group_by(.harness) | map({key: .[0].harness, value: (.[0].version as $v | {version: $v, legs: (map({key: .leg, value: $res[.leg]}) | from_entries), leg_versions: (map({key: .leg, value: .version}) | from_entries), pass: (all(.[]; $res[.leg] == "pass" and .version == $v))})}) | from_entries)'
	;;
compare-claude)
	usage="usage: harness-compat.sh compare-claude HOME"
	home="${2:?$usage}"
	port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
	HOME="$home" ANTHROPIC_BASE_URL="http://127.0.0.1:$port" ANTHROPIC_AUTH_TOKEN=dummy ANTHROPIC_API_KEY='' \
		claude -p hi >/dev/null 2>&1 &
	pid=$!
	trap 'kill "$pid" 2>/dev/null || true' EXIT
	f="$home/.claude/sessions/$pid.json"
	n=0
	while :; do
		why=
		if [ ! -f "$f" ]; then
			why="no session file $f"
		else
			s="$(jq -r --argjson pid "$pid" 'select((.pid == $pid) and (.sessionId | type == "string" and length > 0) and (.kind | type == "string" and length > 0) and (.messagingSocketPath | type == "string" and length > 0)) | .messagingSocketPath' "$f" 2>/dev/null || true)"
			case "$s" in
			"") why="session file lacks the fields the adapter reads: $f" ;;
			/*/cc-socks/"$pid".sock) [ -n "$(find "$s" -maxdepth 0 -type s -user "$(id -u)" 2>/dev/null)" ] || why="socket missing or not owned by this user: $s" ;;
			*) why="socket path is not an absolute .../cc-socks/<pid>.sock for pid $pid: $s" ;;
			esac
		fi
		[ -z "$why" ] && break
		n=$((n + 1))
		[ "$n" -lt "${COMPAT_WAIT_TRIES:-40}" ] || {
			echo "$why" >&2
			exit 1
		}
		sleep 0.5
	done
	echo "claude session file and socket ok"
	;;
compare-codex)
	usage="usage: harness-compat.sh compare-codex HOME"
	home="${2:?$usage}"
	mkdir -p "$home/.codex"
	out="$(HOME="$home" CODEX_HOME="$home/.codex" codex --version)"
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 1 ] && printf '%s\n' "$out" | grep -Eq '^codex-cli [0-9]+\.[0-9]+\.[0-9]+[[:space:]]*$' || {
		echo "codex --version no longer parses: $out" >&2
		exit 1
	}
	port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
	TIMEOUT_CMD=
	if command -v timeout >/dev/null 2>&1; then
		TIMEOUT_CMD="timeout 60"
	elif command -v perl >/dev/null 2>&1; then
		TIMEOUT_CMD="perl -e alarm(60);exec(@ARGV) --"
	fi
	HOME="$home" CODEX_HOME="$home/.codex" OR_KEY=dummy $TIMEOUT_CMD codex exec --skip-git-repo-check \
		-c "model_providers.closed={name=\"closed\",base_url=\"http://127.0.0.1:$port/v1\",env_key=\"OR_KEY\",wire_api=\"responses\"}" \
		-c model_provider=closed -m closed/none hi >/dev/null 2>&1 || true
	q="$home/.codex/queue_1.sqlite"
	[ -f "$q" ] || {
		echo "codex exec did not create $q" >&2
		exit 1
	}
	echo "$q"
	;;
compare-opencode)
	usage="usage: harness-compat.sh compare-opencode HOME [VERSION]"
	home="${2:?$usage}"
	want="${3-}"
	port="${COMPAT_OPENCODE_PORT:-49374}"
	xt="${CROSSTALK_BIN:-$home/.local/bin/crosstalk}"
	mkdir -p "$home/.config" "$home/.local/state" "$home/.local/share" "$home/.cache"
	export HOME="$home" XDG_CONFIG_HOME="$home/.config" XDG_STATE_HOME="$home/.local/state" XDG_DATA_HOME="$home/.local/share" XDG_CACHE_HOME="$home/.cache"
	export OPENCODE_DISABLE_AUTOUPDATE=1 OPENCODE_DISABLE_MODELS_FETCH=1 OPENCODE_DISABLE_CLAUDE_CODE=1
	out="$(opencode --version)"
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 1 ] && printf '%s\n' "$out" | grep -Eq '^opencode v[0-9]+\.[0-9]+\.[0-9]+[[:space:]]*$' || {
		echo "opencode --version no longer parses: $out" >&2
		exit 1
	}
	[ -z "$want" ] || [ "$out" = "opencode v$want" ] || {
		echo "opencode --version is '$out', want the candidate opencode v$want: another opencode is on PATH" >&2
		exit 1
	}
	command -v lsof >/dev/null 2>&1 || {
		echo "lsof not found: cannot check which process owns port $port" >&2
		exit 1
	}
	held="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true)"
	[ -z "$held" ] || {
		echo "port $port is already held by pid $held, not by this check: refusing to use it" >&2
		exit 1
	}
	opencode serve --service ${COMPAT_OPENCODE_PORT:+--port "$COMPAT_OPENCODE_PORT"} >"$home/opencode-serve.log" 2>&1 &
	pid=$!
	trap 'kill "$pid" 2>/dev/null || true' EXIT
	f="$XDG_STATE_HOME/opencode/service.json"
	n=0
	while [ ! -f "$f" ]; do
		n=$((n + 1))
		[ "$n" -lt "${COMPAT_WAIT_TRIES:-40}" ] || {
			echo "service did not write $f" >&2
			exit 1
		}
		sleep 0.5
	done
	[ -n "$(find "$f" -maxdepth 0 -type f -perm 600 -user "$(id -u)" 2>/dev/null)" ] || {
		echo "service.json mode is not 0600 or not owned by this user, or it is a symlink: $f" >&2
		exit 1
	}
	if [ -n "$want" ] && [ "$(jq -r '.version // ""' "$f")" != "$want" ]; then
		echo "service.json version is not the candidate $want" >&2
		exit 1
	fi
	url="$(jq -r 'if (.password | type == "string" and length > 0) then .url // "" else "NO-PASSWORD" end' "$f")" || {
		echo "service.json is not JSON: $f" >&2
		exit 1
	}
	[ "$url" != NO-PASSWORD ] || {
		echo "service.json password is missing or empty" >&2
		exit 1
	}
	printf '%s\n' "$url" | grep -Eq '^http://(127\.0\.0\.1|\[::1\]):[0-9]+/?$' || {
		echo "service.json url is not a loopback http origin: $url" >&2
		exit 1
	}
	uport="$(printf '%s\n' "$url" | sed -E 's|^.*:([0-9]+)/?$|\1|')"
	spid="$(jq -r '.pid | if type == "number" then tostring else "" end' "$f")"
	listener="$(lsof -nP -iTCP:"$uport" -sTCP:LISTEN -t 2>/dev/null || true)"
	[ -n "$spid" ] && [ "$spid" = "$listener" ] && [ "$spid" = "$pid" ] || {
		echo "service.json pid '$spid' is not the listener of port $uport ('$listener') or not the service this check launched ('$pid')" >&2
		exit 1
	}
	auth() { printf 'user = "opencode:%s"\n' "$(jq -r .password "$f")"; }
	ses="$(auth | curl -fsS --max-time 20 --config - -H 'content-type: application/json' --data '{"title":"compat"}' "$url/api/session" 2>/dev/null | jq -r '.id // .data.id // empty' 2>/dev/null || true)"
	printf '%s\n' "$ses" | grep -Eq '^ses_[A-Za-z0-9]+$' || {
		echo "POST /api/session did not return a session id of the shape the harness reads: '$ses'" >&2
		exit 1
	}
	auth | curl -fsS --max-time 20 --config - "$url/api/session" 2>/dev/null | jq -e --arg id "$ses" '[.. | objects | select(.id? == $id)] | length > 0' >/dev/null || {
		echo "GET /api/session does not list the new session $ses" >&2
		exit 1
	}
	[ -x "$xt" ] || {
		echo "no crosstalk binary at $xt" >&2
		exit 1
	}
	req="$(jq -nc --arg id "$ses" '{jsonrpc: "2.0", id: 2, method: "tools/call", params: {name: "list_agents", arguments: {}, _meta: {"ai.opencode/sessionID": $id}}}')"
	init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"compat","version":"0"}}}'
	errlog="$(mktemp)"
	t0="$(date +%s)"
	resp="$( (printf '%s\n' "$init"; sleep 1; printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'; printf '%s\n' "$req"; sleep 3) | "$xt" mcp 2>"$errlog" | jq -c 'select(.id == 2)' )" || true
	printf '%s' "$resp" | jq -e '[.. | objects | select(.harness? == "opencode" and .you? == true and .name? == "compat")] | length == 1' >/dev/null 2>&1 || {
		echo "a tools/call carrying _meta ai.opencode/sessionID $ses did not bind to an opencode row after $(($(date +%s) - t0))s" >&2
		echo "shim stderr:" >&2
		cat "$errlog" >&2
		rm -f "$errlog"
		exit 1
	}
	rm -f "$errlog"
	echo "opencode service.json, session and _meta binding ok"
	;;
pi-wake)
	usage="usage: harness-compat.sh pi-wake SESSIONS_DIR SESSION_ID BODY"
	dir="${2:?$usage}"
	id="${3:?$usage}"
	body="${4:?$usage}"
	wake='A CrossTalk message is waiting: call receive_message'
	f="$(find "$dir" -type f -name "*_$id.jsonl" 2>/dev/null)"
	[ -n "$f" ] || {
		echo "no session file for $id, so the fixed wake was not admitted" >&2
		exit 1
	}
	if grep -rqF -- "$body" "$dir"; then
		echo "the mailboxed body reached the Pi session" >&2
		exit 1
	fi
	jq -es --arg w "$wake" '
		([.[] | select(.type == "custom_message")] | length == 1 and .[0].customType == "crosstalk" and .[0].content == $w)
		and all(.[] | select(.type == "message"); .message.role == "system" or .message.role == "assistant")' "$f" >/dev/null || {
		echo "the session does not hold exactly the fixed wake and nothing else from a peer" >&2
		exit 1
	}
	# Admission, run start and the provider's answer are separate facts: with no model the run starts and fails.
	stop="$(jq -rs '(map(.type == "custom_message") | index(true)) as $i | [.[$i + 1:][] | select(.type == "message" and .message.role == "assistant")] | if length == 0 then "" else .[0].message.stopReason // "none" end' "$f")"
	[ -n "$stop" ] || {
		echo "the wake was admitted but no run started" >&2
		exit 1
	}
	echo "pi wake: admitted=yes run_started=yes provider_stop=$stop"
	[ "$stop" = error ] || {
		echo "the offline run did not fail at the provider: a model answered" >&2
		exit 1
	}
	;;
compare-pi)
	usage="usage: harness-compat.sh compare-pi HOME [VERSION]"
	home="${2:?$usage}"
	want="${3-}"
	xt="${CROSSTALK_BIN:-$home/.local/bin/crosstalk}"
	state="$home/.local/state/crosstalk"
	mkdir -p "$home/work"
	export HOME="$home" PI_CODING_AGENT_DIR="$home/.pi/agent" PI_OFFLINE=1 PI_TELEMETRY=0
	# Any inherited credential would let the offline wake reach a real provider.
	for v in $(env | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' | grep -E '(_API_KEY|_TOKEN|_SECRET|_ACCESS_KEY|_CREDENTIALS)$|^(AWS|AZURE|GOOGLE|ANTHROPIC|OPENAI)_'); do
		unset "$v"
	done
	unset PI_CODING_AGENT_SESSION_DIR
	out="$(pi --version)"
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 1 ] && semver "$out" || {
		echo "pi --version no longer parses: $out" >&2
		exit 1
	}
	[ -z "$want" ] || [ "$out" = "$want" ] || {
		echo "pi --version is '$out', want the candidate $want: another pi is on PATH" >&2
		exit 1
	}
	[ -f "$PI_CODING_AGENT_DIR/extensions/crosstalk/index.ts" ] || {
		echo "no extension under $PI_CODING_AGENT_DIR: run crosstalk install pi first" >&2
		exit 1
	}
	[ -x "$xt" ] || {
		echo "no crosstalk binary at $xt" >&2
		exit 1
	}
	descs() { cat "$state"/agents/*.json 2>/dev/null | jq -cs 'map(select(.harness == "pi"))'; }
	field() { descs | jq -r --arg k "$1" 'if length == 1 then .[0][$k] // "" else "" end'; }
	rows() {
		init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"compat","version":"0"}}}'
		call='{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_agents","arguments":{}}}'
		(printf '%s\n' "$init"; sleep 1; printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$call"; sleep 3) |
			"$xt" mcp 2>"$home/list-shim.log" | jq -ce 'select(.id == 2) | [.result.structuredContent.agents[] | select(.harness == "pi")]'
	}
	wait_for() {
		n=0
		until "$@"; do
			n=$((n + 1))
			[ "$n" -lt "${COMPAT_WAIT_TRIES:-60}" ] || return 1
			sleep 0.5
		done
	}
	pid=
	die() {
		echo "$1" >&2
		echo "pty output (last 40 lines):" >&2
		tail -n 40 "$home/pi-pty.log" >&2 2>/dev/null || true
		exit 1
	}
	# The cat that feeds the pty holds the FIFO open itself, so it never sees EOF.
	trap '[ -z "$pid" ] || { pkill -P "$pid" || true; kill "$pid" 2>/dev/null || true; }; pkill -P $$ -x cat || true' EXIT
	[ "$(descs)" = "[]" ] || {
		echo "a pi descriptor exists before pi started: $home is not a fresh scratch home" >&2
		exit 1
	}
	mkfifo "$home/pi-keys"
	exec 7<>"$home/pi-keys"
	# Pi needs a terminal, and BSD script refuses a FIFO as its stdin, so the keys arrive through a pipe.
	run='stty rows 40 cols 120 && exec pi'
	cd "$home/work"
	if script --version 2>&1 | grep -q util-linux; then
		cat <&7 2>/dev/null | script -qfec "$run" /dev/null >"$home/pi-pty.log" 2>&1 &
	else
		cat <&7 2>/dev/null | script -q /dev/null sh -c "$run" >"$home/pi-pty.log" 2>&1 &
	fi
	pid=$!
	type_line() {
		printf '%s' "$1" >&7
		sleep 1
		printf '\r' >&7
	}
	registered() { [ -n "$(field session_ref)" ]; }
	wait_for registered || die "pi did not register a session"
	ref="$(field session_ref)"
	shim="$(field pid)"
	host="$(field harness_pid)"
	parent="$(ps -o ppid= -p "$shim" | tr -d ' ')"
	[ "$parent" = "$host" ] || die "the shim's parent pid '$parent' is not the registered Pi pid '$host'"
	echo "E3 and E6 ok on $(uname -s) $(uname -m): the shim's parent is Pi (pid $host) and its socket accepted the extension's hello"
	listed="$(rows)" || die "list_agents through a second shim returned nothing"
	printf '%s' "$listed" | jq -e 'length == 1 and .[0].reachable == true' >/dev/null || die "a second shim does not list the pi session as reachable: $listed"
	type_line /new
	renewed() { r="$(field session_ref)" && [ -n "$r" ] && [ "$r" != "$ref" ]; }
	wait_for renewed || die "/new did not register a new session"
	gone() { ! kill -0 "$1" 2>/dev/null; }
	wait_for gone "$shim" || die "/new left the old shim $shim running"
	ref="$(field session_ref)"
	shim="$(field pid)"
	[ "$(field harness_pid)" = "$host" ] || die "/new changed the Pi pid"
	echo "E4 ok: /new ended the old shim and registered a new session from the same Pi process"
	agent="$(field agent_id)"
	body="compat-pi-body-$$"
	# crosstalk send exits 1 for any state but accepted or queued.
	ack="$("$xt" send "$agent" "$body")" || true
	mid="$(printf '%s' "$ack" | jq -r 'select(.state == "mailboxed") | .message_id')"
	[ -n "$mid" ] || die "a script send did not answer mailboxed: $ack"
	jq -es --arg m "$mid" --arg b "$body" 'any(.[]; .message_id == $m and any(.message.parts[]?; .text == $b)) and any(.[]; .message_id == $m and .state == "mailboxed")' "$state/journal/$agent.jsonl" >/dev/null ||
		die "the journal does not hold the mailboxed body of $mid"
	woke() { "$0" pi-wake "$PI_CODING_AGENT_DIR/sessions" "${ref%@*}" "$body" >"$home/pi-wake.log" 2>&1; }
	# $0 is relative to the starting directory.
	cd "$OLDPWD"
	wait_for woke || die "$(cat "$home/pi-wake.log")"
	cat "$home/pi-wake.log"
	type_line /quit
	empty() { [ "$(descs)" = "[]" ]; }
	wait_for empty || die "the pi session is still registered after pi quit"
	wait_for gone "$shim" || die "the shim is still running after pi quit"
	wait_for gone "$host" || die "the Pi process is still running after /quit"
	listed="$(rows)" || die "list_agents through a second shim returned nothing"
	[ "$listed" = "[]" ] || die "a second shim still lists a pi session after pi quit: $listed"
	echo "pi registration, /new, mailbox wake and quit ok"
	;;
passing | failing)
	usage="usage: harness-compat.sh $1 RESULTS_JSON"
	want=true
	[ "$1" = failing ] && want=false
	printf '%s' "${2:?$usage}" | jq -r --argjson want "$want" 'to_entries[] | select((.value.pass == true) == $want) | .key + " " + .value.version'
	;;
bump-record)
	usage="usage: harness-compat.sh bump-record RECORD HARNESS VERSION DATE"
	record="${2:?$usage}"
	h="${3:?$usage}"
	v="${4:?$usage}"
	d="${5:?$usage}"
	pkg "$h" >/dev/null
	semver "$v" || {
		echo "version must be x.y.z: $v" >&2
		exit 1
	}
	printf '%s\n' "$d" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}$' || {
		echo "date must be YYYY-MM-DD: $d" >&2
		exit 1
	}
	out="$(jq -r --arg h "$h" --arg v "$v" --arg d "$d" '
		.[$h] = {tested: $v, checked: $d}
		| "{\n" + (to_entries | map("  \(.key | @json): {\"tested\": \(.value.tested | @json), \"checked\": \(.value.checked | @json)}") | join(",\n")) + "\n}"' "$record")"
	printf '%s\n' "$out" >"$record"
	;;
failed-steps)
	usage="usage: harness-compat.sh failed-steps JOBS_FILE codex|claude-code|opencode|pi"
	case "${3:?$usage}" in
	claude-code) own=claude ;;
	codex) own=codex ;;
	opencode) own=opencode ;;
	pi) own=pi ;;
	*)
		echo "unknown harness: $3" >&2
		exit 1
		;;
	esac
	jq -rs --arg o "$own" '
		map(select(.conclusion == "failure" and ((.name | capture("^(compare|harness)-(?<h>claude|codex|opencode|pi)-").h // $o) == $o)))
		| map(.name as $j | ([.steps[]? | select(.conclusion == "failure") | .name] | if length == 0 then ["(no step recorded)"] else . end) | map($j + ": " + .)) | add // [] | .[]' "${2:?$usage}"
	;;
tested-code)
	usage="usage: harness-compat.sh tested-code RUN_SHA BASE_REF RECORD"
	changed="$(git diff --name-only "${2:?$usage}" "${3:?$usage}")" || {
		echo "git diff $2 $3 failed" >&2
		exit 1
	}
	extra="$(printf '%s\n' "$changed" | grep -vxF "${4:?$usage}" || true)"
	[ -z "$extra" ] || {
		echo "$3 changed since the tested revision $2; rerun to test it: $extra" >&2
		exit 1
	}
	;;
issue-title)
	usage="usage: harness-compat.sh issue-title HARNESS VERSION"
	echo "harness-compat: ${2:?$usage} ${3:?$usage} did not pass"
	;;
issue-body)
	usage="usage: harness-compat.sh issue-body VERSION RUN_URL [FAILED_STEP...]"
	v="${2:?$usage}"
	url="${3:?$usage}"
	shift 3
	printf 'The scheduled harness-compat run did not pass for version %s.\n\nFailing steps:\n' "$v"
	if [ "$#" -eq 0 ]; then echo "- none recorded (a job was skipped or cancelled)"; fi
	for s in "$@"; do printf -- '- %s\n' "$s"; done
	printf '\nRun: %s\n' "$url"
	;;
file-issue)
	usage="usage: harness-compat.sh file-issue REPO HARNESS VERSION RUN_URL JOBS_FILE"
	repo="${2:?$usage}"
	h="${3:?$usage}"
	v="${4:?$usage}"
	url="${5:?$usage}"
	jobs="${6:?$usage}"
	pkg "$h" >/dev/null
	semver "$v" || {
		echo "version must be x.y.z: $v" >&2
		exit 1
	}
	title="$("$0" issue-title "$h" "$v")"
	listed="$(gh issue list --repo "$repo" --state open --search "\"$title\" in:title" --json number,title)" || {
		echo "cannot list issues; not filing a possible duplicate" >&2
		exit 1
	}
	open="$(printf '%s' "$listed" | jq -r --arg t "$title" '[.[] | select(.title == $t)] | .[0].number // empty')"
	if [ -n "$open" ]; then
		echo "reusing issue #$open for $h $v"
		exit 0
	fi
	steps="$("$0" failed-steps "$jobs" "$h")"
	body="$(
		IFS='
'
		set -f
		# shellcheck disable=SC2086
		set -- $steps
		"$0" issue-body "$v" "$url" "$@"
	)"
	gh issue create --repo "$repo" --title "$title" --label harness-compat --body "$body" ||
		{
			echo "label harness-compat is missing; filing without it" >&2
			gh issue create --repo "$repo" --title "$title" --body "$body"
		}
	;;
*)
	echo "usage: harness-compat.sh check-updates|legs|step-status|report|compare-claude|compare-codex|compare-opencode|compare-pi|pi-wake|passing|failing|bump-record|tested-code|failed-steps|issue-title|issue-body|file-issue ..." >&2
	exit 2
	;;
esac
