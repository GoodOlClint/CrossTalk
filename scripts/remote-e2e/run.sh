#!/usr/bin/env bash
# Remote end-to-end scenario: two containers, real remote config/serve/pair/join, one inform, one ask and reply.
# Usage: run.sh BIN_DIR (holds linux crosstalk and remotee2e binaries). FAIL_PAIRING=1 joins with a wrong token and must exit non-zero.
# Prints one line: REMOTE_E2E inform_seconds=N reply_seconds=N
set -euo pipefail

BIN=${1:?bin dir}
HERE=$(cd "$(dirname "$0")" && pwd)
LOGS=${LOGS:-$PWD/e2e-logs}
IMG=xt-e2e
NET=xt-e2e-net
SUBNET=172.30.0.0/24
IP_A=172.30.0.10
IP_B=172.30.0.11
mkdir -p "$LOGS"

now() { date +%s.%N; }
dx() { local c=$1; shift; docker exec "$c" "$@"; }

cleanup() {
	for c in xt-a xt-b; do
		docker cp "$c:/home/x/out" "$LOGS/$c" >/dev/null 2>&1 || true
		rm -f "$LOGS/$c/pair.out"
		docker rm -f "$c" >/dev/null 2>&1 || true
	done
	docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

t=$(now)
cp "$HERE/Dockerfile" "$BIN/Dockerfile"
docker build -q -t "$IMG" "$BIN" >/dev/null
echo "image_build_seconds=$(echo "$(now) - $t" | bc)"

docker network rm "$NET" >/dev/null 2>&1 || true
docker network create --subnet "$SUBNET" "$NET" >/dev/null
for h in a b; do
	ip=IP_${h^^}
	docker run -d --name "xt-$h" --network "$NET" --ip "${!ip}" "$IMG" >/dev/null
	dx "xt-$h" mkdir -p /home/x/out
done

start_host() { # container ip alias agent
	dx "$1" crosstalk remote config --bind "$2" --alias "$3" --yes >/dev/null
	docker exec -d "$1" sh -c 'exec crosstalk remote serve >/home/x/out/serve.log 2>&1'
	docker exec -d "$1" sh -c "exec remotee2e agent $4 >/home/x/out/agent.log 2>&1"
}
start_host xt-a "$IP_A" hosta alice
start_host xt-b "$IP_B" hostb bob

wait_for() { # description timeout command...
	local what=$1 limit=$2 i=0
	shift 2
	until "$@" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt $((limit * 5)) ] || { echo "timeout waiting for $what" >&2; exit 1; }
		sleep 0.2
	done
}
wait_for "alice ready" 30 docker exec xt-a grep -q ready /home/x/out/agent.log
wait_for "bob ready" 30 docker exec xt-b grep -q ready /home/x/out/agent.log
wait_for "gateway a" 30 docker exec xt-a grep -qi listen /home/x/out/serve.log || true

T0=$(now)
docker exec -d xt-a sh -c 'exec crosstalk remote pair --alias hostb >/home/x/out/pair.out 2>/home/x/out/pair.err'
wait_for "token" 30 docker exec xt-a grep -q . /home/x/out/pair.out
TOKEN=$(dx xt-a head -n1 /home/x/out/pair.out)
if [ "${FAIL_PAIRING:-0}" = 1 ]; then TOKEN=wrong-token; fi
echo "$TOKEN" | docker exec -i xt-b crosstalk remote join --alias hosta >"$LOGS/join.out" 2>"$LOGS/join.err"
wait_for "paired" 30 docker exec xt-a grep -q "paired with" /home/x/out/pair.out
T_PAIRED=$(now)

dx xt-a remotee2e call send_message '{"to":"bob@hostb","text":"hello from a"}' | tee "$LOGS/inform.json"
wait_for "one inject on b" 30 sh -c "docker exec xt-b remotee2e writes | grep -q hello"
T_INFORM=$(now)
N=$(dx xt-b remotee2e writes | tee "$LOGS/b-writes.json" | grep -o '"message_id"' | wc -l)
[ "$N" -eq 1 ] || { echo "receiver has $N injects, want 1" >&2; exit 1; }
sleep 3
N=$(dx xt-b remotee2e writes | grep -o '"message_id"' | wc -l)
[ "$N" -eq 1 ] || { echo "receiver has $N injects after settle, want 1" >&2; exit 1; }

dx xt-a remotee2e call ask '{"to":"bob@hostb","text":"question","deadline_seconds":120}' | tee "$LOGS/ask.json"
wait_for "ask injected on b" 30 sh -c "docker exec xt-b remotee2e writes | grep -q question"
ASK_ID=$(dx xt-b remotee2e writes | jq -r '.[] | select(.text=="question") | .message_id')
dx xt-b remotee2e call reply "{\"message_id\":\"$ASK_ID\",\"text\":\"42\"}" | tee "$LOGS/reply.json"
wait_for "reply injected on a" 30 sh -c "docker exec xt-a remotee2e writes | grep -q 42"
T_REPLY=$(now)
sleep 3
for h in a b; do
	dx "xt-$h" remotee2e writes >"$LOGS/$h-writes-final.json"
done
[ "$(jq 'length' "$LOGS/a-writes-final.json")" -eq 1 ] || { echo "asker has wrong inject count" >&2; exit 1; }
[ "$(jq '[.[] | select(.text=="question")] | length' "$LOGS/b-writes-final.json")" -eq 1 ] || { echo "receiver injected the ask more than once" >&2; exit 1; }
REPLY_S=$(echo "$T_REPLY - $T0" | bc)

echo "pair_seconds=$(echo "$T_PAIRED - $T0" | bc)"
echo "REMOTE_E2E inform_seconds=$(echo "$T_INFORM - $T0" | bc) reply_seconds=$REPLY_S"
if [ "$(echo "$REPLY_S < 180" | bc)" != 1 ]; then
	echo "ask and reply took $REPLY_S s, pass line is under 180 s" >&2
	exit 1
fi
