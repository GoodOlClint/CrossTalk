#!/usr/bin/env bash
# Remote end-to-end scenario: two containers, real remote config/serve/pair/join, one inform, one ask and reply.
# Usage: run.sh BIN_DIR (holds linux crosstalk and remotee2e binaries). FAIL_PAIRING=1 joins with a wrong token and must exit non-zero.
# Prints one line: REMOTE_E2E inform_seconds=N reply_seconds=N
set -euo pipefail

BIN=${1:?bin dir}
HERE=$(cd "$(dirname "$0")" && pwd)
LOGS=${LOGS:-$PWD/e2e-logs}
RUN=$$
IMG=xt-e2e
NET=xt-e2e-net-$RUN
CA=xt-a-$RUN
CB=xt-b-$RUN
SUBNET=172.30.0.0/24
IP_A=172.30.0.10
IP_B=172.30.0.11
mkdir -p "$LOGS"

now() { date +%s.%N; }
dx() { local c=$1; shift; docker exec "$c" "$@"; }

CREATED=()
NET_MADE=0
cleanup() {
	local c
	for c in ${CREATED[@]+"${CREATED[@]}"}; do
		docker exec "$c" rm -f /home/x/out/pair.out >/dev/null 2>&1 || true
		rm -rf "$LOGS/${c%-"$RUN"}"
		docker cp "$c:/home/x/out" "$LOGS/${c%-"$RUN"}" >/dev/null 2>&1 || true
		rm -f "$LOGS/${c%-"$RUN"}/pair.out"
		docker rm -f "$c" >/dev/null 2>&1 || true
	done
	if [ "$NET_MADE" = 1 ]; then docker network rm "$NET" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

t=$(now)
cp "$HERE/Dockerfile" "$BIN/Dockerfile"
docker build -q -t "$IMG" "$BIN" >/dev/null
echo "image_build_seconds=$(echo "$(now) - $t" | bc)"

docker network create --subnet "$SUBNET" "$NET" >/dev/null
NET_MADE=1
for h in a b; do
	ip=IP_${h^^}
	c=xt-$h-$RUN
	docker run -d --name "$c" --network "$NET" --ip "${!ip}" "$IMG" >/dev/null
	CREATED+=("$c")
	dx "$c" mkdir -p /home/x/out
done

start_host() { # container ip alias agent
	dx "$1" crosstalk remote config --bind "$2" --alias "$3" --yes >/dev/null
	docker exec -d "$1" sh -c 'exec crosstalk remote serve >/home/x/out/serve.log 2>&1'
	docker exec -d "$1" sh -c "exec remotee2e agent $4 >/home/x/out/agent.log 2>&1"
}
start_host $CA "$IP_A" hosta alice
start_host $CB "$IP_B" hostb bob

wait_for() { # description timeout command...
	local what=$1 limit=$2 i=0
	shift 2
	until "$@" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt $((limit * 5)) ] || { echo "timeout waiting for $what" >&2; exit 1; }
		sleep 0.2
	done
}
wait_for "alice ready" 30 docker exec $CA grep -q ready /home/x/out/agent.log
wait_for "bob ready" 30 docker exec $CB grep -q ready /home/x/out/agent.log
wait_for "gateway a" 30 docker exec $CA grep -qi listen /home/x/out/serve.log || true

T0=$(now)
docker exec -d $CA sh -c 'exec crosstalk remote pair --alias hostb >/home/x/out/pair.out 2>/home/x/out/pair.err'
wait_for "token" 30 docker exec $CA grep -q . /home/x/out/pair.out
TOKEN=$(dx $CA head -n1 /home/x/out/pair.out)
if [ "${FAIL_PAIRING:-0}" = 1 ]; then TOKEN=wrong-token; fi
echo "$TOKEN" | docker exec -i $CB crosstalk remote join --alias hosta >"$LOGS/join.out" 2>"$LOGS/join.err" || { echo "remote join failed:" >&2; cat "$LOGS/join.err" >&2; exit 1; }
wait_for "paired" 30 docker exec $CA grep -q "paired with" /home/x/out/pair.out
T_PAIRED=$(now)

dx $CA remotee2e call send_message '{"to":"bob@hostb","text":"hello from a"}' | tee "$LOGS/inform.json"
wait_for "one inject on b" 30 sh -c "docker exec $CB remotee2e writes | grep -q hello"
T_INFORM=$(now)
N=$(dx $CB remotee2e writes | tee "$LOGS/b-writes.json" | grep -o '"message_id"' | wc -l)
[ "$N" -eq 1 ] || { echo "receiver has $N injects, want 1" >&2; exit 1; }
sleep 3
N=$(dx $CB remotee2e writes | grep -o '"message_id"' | wc -l)
[ "$N" -eq 1 ] || { echo "receiver has $N injects after settle, want 1" >&2; exit 1; }

dx $CA remotee2e call ask '{"to":"bob@hostb","text":"question","deadline_seconds":120}' | tee "$LOGS/ask.json"
wait_for "ask injected on b" 30 sh -c "docker exec $CB remotee2e writes | grep -q question"
ASK_ID=$(dx $CB remotee2e writes | jq -r '.[] | select(.text=="question") | .message_id')
ASK_SENT=$(jq -r '.message_id' "$LOGS/ask.json")
[ "$ASK_ID" = "$ASK_SENT" ] || { echo "receiver saw ask id $ASK_ID, sender sent $ASK_SENT" >&2; exit 1; }
dx $CB remotee2e call reply "{\"message_id\":\"$ASK_ID\",\"text\":\"42\"}" | tee "$LOGS/reply.json"
wait_for "reply injected on a" 30 sh -c "docker exec $CA remotee2e writes | jq -e 'any(.[]; .text==\"42\")'"
T_REPLY=$(now)
sleep 3
for h in a b; do
	dx "xt-$h-$RUN" remotee2e writes >"$LOGS/$h-writes-final.json"
done
for f in inform ask reply; do
	[ "$(jq -r '.is_error' "$LOGS/$f.json")" = false ] || { echo "$f tool call reported an error" >&2; exit 1; }
done
[ "$(jq 'length' "$LOGS/a-writes-final.json")" -eq 1 ] || { echo "asker has wrong inject count" >&2; exit 1; }
REPLY_ID=$(jq -r '.message_id' "$LOGS/reply.json")
[ "$(jq --arg id "$ASK_SENT" --arg rid "$REPLY_ID" '[.[] | select(.text=="42" and .in_reply_to==$id and .from=="bob@hostb" and .message_id==$rid)] | length' "$LOGS/a-writes-final.json")" -eq 1 ] || { echo "asker lacks exactly one reply to the ask" >&2; exit 1; }
[ "$(jq '[.[] | select(.text=="question")] | length' "$LOGS/b-writes-final.json")" -eq 1 ] || { echo "receiver injected the ask more than once" >&2; exit 1; }
REPLY_S=$(echo "$T_REPLY - $T0" | bc)

echo "pair_seconds=$(echo "$T_PAIRED - $T0" | bc)"
echo "REMOTE_E2E inform_seconds=$(echo "$T_INFORM - $T0" | bc) reply_seconds=$REPLY_S"
if [ "$(echo "$REPLY_S < 180" | bc)" != 1 ]; then
	echo "ask and reply took $REPLY_S s, pass line is under 180 s" >&2
	exit 1
fi
