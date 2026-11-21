#!/bin/sh
set -eu

pkg() {
	case "$1" in
	codex) echo @openai/codex ;;
	claude-code) echo @anthropic-ai/claude-code ;;
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
detect)
	usage="usage: harness-compat.sh detect RECORD HARNESS VERSION FORCE RECORDED"
	record="${2:?$usage}"
	which="${3:?$usage}"
	forced="${4-}"
	force="${5-}"
	recorded="${6-}"
	case "$which" in all) set -- codex claude-code ;; *) set -- "$which" ;; esac
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
tier0-claude)
	usage="usage: harness-compat.sh tier0-claude HOME"
	home="${2:?$usage}"
	sock=/tmp/cc-socks
	port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
	HOME="$home" ANTHROPIC_BASE_URL="http://127.0.0.1:$port" ANTHROPIC_AUTH_TOKEN=dummy ANTHROPIC_API_KEY= \
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
			"$sock"/*) [ -S "$s" ] || why="socket missing: $s" ;;
			*) why="socket outside $sock: $s" ;;
			esac
		fi
		[ -z "$why" ] && break
		n=$((n + 1))
		[ "$n" -lt 40 ] || {
			echo "$why" >&2
			exit 1
		}
		sleep 0.5
	done
	echo "claude session file and socket ok"
	;;
tier0-codex)
	usage="usage: harness-compat.sh tier0-codex HOME"
	home="${2:?$usage}"
	mkdir -p "$home/.codex"
	out="$(HOME="$home" CODEX_HOME="$home/.codex" codex --version)"
	[ "$(printf '%s\n' "$out" | wc -l)" -eq 1 ] && printf '%s\n' "$out" | grep -Eq '^codex-cli [0-9]+\.[0-9]+\.[0-9]+[[:space:]]*$' || {
		echo "codex --version no longer parses: $out" >&2
		exit 1
	}
	port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
	TIMEOUT_CMD=
	command -v timeout >/dev/null 2>&1 && TIMEOUT_CMD="timeout 60"
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
*)
	echo "usage: harness-compat.sh detect|tier0-claude|tier0-codex ..." >&2
	exit 2
	;;
esac
