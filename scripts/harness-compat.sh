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
	n=0
	f="$home/.claude/sessions/$pid.json"
	while [ ! -f "$f" ] && [ "$n" -lt 40 ]; do
		sleep 0.5
		n=$((n + 1))
	done
	trap 'kill "$pid" 2>/dev/null || true' EXIT
	[ -f "$f" ] || {
		echo "no session file $f" >&2
		exit 1
	}
	jq -e --argjson pid "$pid" '(.pid == $pid) and (.sessionId | type == "string" and length > 0) and (.kind | type == "string" and length > 0) and (.messagingSocketPath | type == "string" and length > 0)' "$f" >/dev/null || {
		echo "session file lacks the fields the adapter reads: $f" >&2
		exit 1
	}
	s="$(jq -r .messagingSocketPath "$f")"
	case "$s" in "$sock"/*) ;; *)
		echo "socket outside $sock: $s" >&2
		exit 1
		;;
	esac
	[ -S "$s" ] || {
		echo "socket missing: $s" >&2
		exit 1
	}
	echo "claude session file and socket ok"
	;;
tier0-codex)
	usage="usage: harness-compat.sh tier0-codex HOME"
	home="${2:?$usage}"
	mkdir -p "$home/.codex"
	out="$(HOME="$home" CODEX_HOME="$home/.codex" codex --version)"
	printf '%s\n' "$out" | grep -Eq '^codex-cli [0-9]+\.[0-9]+\.[0-9]+[[:space:]]*$' || {
		echo "codex --version no longer parses: $out" >&2
		exit 1
	}
	port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
	HOME="$home" CODEX_HOME="$home/.codex" OR_KEY=dummy timeout 60 codex exec --skip-git-repo-check \
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
