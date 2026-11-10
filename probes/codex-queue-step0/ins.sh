#!/bin/sh
# usage: ins.sh <home> <thread> <text> [order]
H=$1; T=$2; X=$3; Q=$H/queue_1.sqlite
ID=$(python3 -c 'import uuid;print(uuid.uuid4())'); CID=$(python3 -c 'import uuid;print(uuid.uuid4())')
NOW=$(python3 -c 'import time;print(int(time.time()*1000))')
ORD=${4:-$(sqlite3 $Q "select coalesce(max(queue_order)+1,0) from queued_items where thread_id='$T'")}
P=$(python3 -c "import json,sys;print(json.dumps({'UserInput':{'content':[{'type':'text','text':sys.argv[1],'text_elements':[]}],'client_id':sys.argv[2]}},separators=(',',':')))" "$X" "$CID")
sqlite3 $Q "PRAGMA busy_timeout=5000; BEGIN IMMEDIATE; INSERT INTO queued_items(id,thread_id,payload_json,queue_order,created_at_ms,updated_at_ms) VALUES('$ID','$T','$(echo "$P" | sed "s/'/''/g")',$ORD,$NOW,$NOW); COMMIT;"
echo "inserted $ID order=$ORD at $(python3 -c 'import time;print(time.time())')"
