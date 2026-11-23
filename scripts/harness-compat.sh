#!/bin/sh
set -eu

pkg() {
	case "$1" in
	codex) echo @openai/codex ;;
	claude-code) echo @anthropic-ai/claude-code ;;
	opencode) echo @opencode/cli ;;
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
	case "$which" in all) set -- codex claude-code opencode ;; *) set -- "$which" ;; esac
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
	printf '%s' "${2:?$usage}" | jq -c --arg q "${3-}" '[.[] | (if .harness == "claude-code" then ({leg: "claude-linux", harness: .harness, version: .version, runner: "ubuntu-latest"}, {leg: "claude-macos", harness: .harness, version: .version, runner: "macos-latest"}) elif .harness == "codex" then ({leg: "codex-linux", harness: .harness, version: .version, runner: "ubuntu-latest"}, {leg: "codex-macos", harness: .harness, version: (if $q != "" then $q else .version end), runner: "macos-latest"}) elif .harness == "opencode" then ({leg: "opencode-linux", harness: .harness, version: .version, runner: "ubuntu-latest"}, {leg: "opencode-macos", harness: .harness, version: .version, runner: "macos-latest"}) else empty end)]'
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
	resp="$( (printf '%s\n' "$init"; sleep 1; printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'; printf '%s\n' "$req"; sleep 3) | "$xt" mcp 2>/dev/null | jq -c 'select(.id == 2)' )" || true
	printf '%s' "$resp" | jq -e '[.. | objects | select(.harness? == "opencode" and .you? == true and .name? == "compat")] | length == 1' >/dev/null 2>&1 || {
		echo "a tools/call carrying _meta ai.opencode/sessionID $ses did not bind to an opencode row" >&2
		exit 1
	}
	echo "opencode service.json, session and _meta binding ok"
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
	usage="usage: harness-compat.sh failed-steps JOBS_FILE codex|claude-code|opencode"
	case "${3:?$usage}" in
	claude-code) own=claude ;;
	codex) own=codex ;;
	opencode) own=opencode ;;
	*)
		echo "unknown harness: $3" >&2
		exit 1
		;;
	esac
	jq -rs --arg o "$own" '
		map(select(.conclusion == "failure" and ((.name | capture("^(compare|harness)-(?<h>claude|codex|opencode)-").h // $o) == $o)))
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
	echo "usage: harness-compat.sh check-updates|legs|step-status|report|compare-claude|compare-codex|compare-opencode|passing|failing|bump-record|tested-code|failed-steps|issue-title|issue-body|file-issue ..." >&2
	exit 2
	;;
esac
