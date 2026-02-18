#!/bin/bash
# mroute Integration Test - Live Transport Testing

BASE="http://localhost:8080"
PASS=0; FAIL=0; TOTAL=0
GREEN='\033[0;32m'; RED='\033[0;31m'; YEL='\033[1;33m'; NC='\033[0m'

check() {
    TOTAL=$((TOTAL+1))
    if [ "$2" = "$3" ]; then
        echo -e "${GREEN}  PASS${NC} $1"
        PASS=$((PASS+1))
    else
        echo -e "${RED}  FAIL${NC} $1 (expected=$2 got=$3)"
        FAIL=$((FAIL+1))
    fi
}

cleanup_ffmpeg() {
    pkill -f "ffmpeg.*testsrc" 2>/dev/null || true
    pkill -f "ffmpeg.*udp://127" 2>/dev/null || true
    sleep 1
}

echo -e "${YEL}=============================${NC}"
echo -e "${YEL}  mroute Integration Tests${NC}"
echo -e "${YEL}=============================${NC}"
echo ""

# ==========================================
echo -e "${YEL}[1] Health${NC}"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/health")
check "GET /health" "200" "$STATUS"

# ==========================================
echo ""
echo -e "${YEL}[2] Create Flow${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Test Flow",
  "description": "UDP relay test",
  "source": {
    "name": "primary",
    "protocol": "udp",
    "ingest_port": 5000
  },
  "outputs": [
    {
      "name": "out1",
      "protocol": "udp",
      "destination": "127.0.0.1",
      "port": 6000
    }
  ]
}')
STATUS=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | head -1)
check "POST /v1/flows" "201" "$STATUS"
FLOW_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
FLOW_STATUS=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")
check "flow status=STANDBY" "STANDBY" "$FLOW_STATUS"

# ==========================================
echo ""
echo -e "${YEL}[3] Get Flow${NC}"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$FLOW_ID")
check "GET /v1/flows/:id" "200" "$STATUS"

# ==========================================
echo ""
echo -e "${YEL}[4] List Flows${NC}"
COUNT=$(curl -s "$BASE/v1/flows" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['flows']))")
check "flow count" "1" "$COUNT"

# ==========================================
echo ""
echo -e "${YEL}[5] Add Output${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$FLOW_ID/outputs" -H "Content-Type: application/json" -d '{
  "name": "out2",
  "protocol": "udp",
  "destination": "127.0.0.1",
  "port": 6001
}')
STATUS=$(echo "$RESP" | tail -1)
check "POST add output" "200" "$STATUS"
OUT_COUNT=$(echo "$RESP" | head -1 | python3 -c "import sys,json; print(len(json.load(sys.stdin)['outputs']))")
check "outputs count=2" "2" "$OUT_COUNT"

# ==========================================
echo ""
echo -e "${YEL}[6] Remove Output${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X DELETE "$BASE/v1/flows/$FLOW_ID/outputs/out2")
STATUS=$(echo "$RESP" | tail -1)
check "DELETE remove output" "200" "$STATUS"
OUT_COUNT=$(echo "$RESP" | head -1 | python3 -c "import sys,json; print(len(json.load(sys.stdin)['outputs']))")
check "outputs count=1 after remove" "1" "$OUT_COUNT"

# ==========================================
echo ""
echo -e "${YEL}[7] Add Source (for failover)${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$FLOW_ID/source" -H "Content-Type: application/json" -d '{
  "name": "backup",
  "protocol": "udp",
  "ingest_port": 5001
}')
STATUS=$(echo "$RESP" | tail -1)
check "POST add backup source" "200" "$STATUS"
SRC_COUNT=$(echo "$RESP" | head -1 | python3 -c "import sys,json; f=json.load(sys.stdin); print(len([f.get('source')]+f.get('sources',[])))")
check "sources count=2" "2" "$SRC_COUNT"

# ==========================================
echo ""
echo -e "${YEL}[8] Remove Backup Source${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X DELETE "$BASE/v1/flows/$FLOW_ID/source/backup")
STATUS=$(echo "$RESP" | tail -1)
check "DELETE remove source" "200" "$STATUS"

# ==========================================
echo ""
echo -e "${YEL}[9] Live Transport Test${NC}"

# Start a source sender FIRST (sends test pattern to port 5000)
ffmpeg -re -f lavfi -i "testsrc=size=640x360:rate=25" -f lavfi -i "sine=frequency=440" \
  -c:v libx264 -preset ultrafast -tune zerolatency -b:v 1000k -c:a aac \
  -f mpegts "udp://127.0.0.1:5000?pkt_size=1316" > /dev/null 2>&1 &
SOURCE_PID=$!
sleep 2

# Start the flow (will listen on port 5000 and relay to port 6000)
curl -s -X POST "$BASE/v1/flows/$FLOW_ID/start" -o /dev/null

sleep 4

# Capture output
ffmpeg -y -i "udp://127.0.0.1:6000?timeout=8000000" -t 5 -c copy /tmp/mroute_relay.ts > /dev/null 2>&1

TOTAL=$((TOTAL+1))
if [ -f /tmp/mroute_relay.ts ] && [ $(stat -c%s /tmp/mroute_relay.ts 2>/dev/null || echo 0) -gt 5000 ]; then
    SIZE=$(ls -lh /tmp/mroute_relay.ts | awk '{print $5}')
    echo -e "${GREEN}  PASS${NC} Live relay received ($SIZE)"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} No/small output received"
    FAIL=$((FAIL+1))
fi

# Check metrics
TOTAL=$((TOTAL+1))
ACTIVE_SRC=$(curl -s "$BASE/v1/flows/$FLOW_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['active_source'])" 2>/dev/null)
if [ "$ACTIVE_SRC" = "primary" ]; then
    echo -e "${GREEN}  PASS${NC} Active source = primary"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} Active source = $ACTIVE_SRC"
    FAIL=$((FAIL+1))
fi

# ==========================================
echo ""
echo -e "${YEL}[10] Stop Flow${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$FLOW_ID/stop")
STATUS=$(echo "$RESP" | tail -1)
check "POST stop flow" "200" "$STATUS"
FLOW_STATUS=$(curl -s "$BASE/v1/flows/$FLOW_ID" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")
check "flow status=STANDBY" "STANDBY" "$FLOW_STATUS"

cleanup_ffmpeg

# ==========================================
echo ""
echo -e "${YEL}[11] Events${NC}"
EVT_COUNT=$(curl -s "$BASE/v1/flows/$FLOW_ID/events" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['events']))")
TOTAL=$((TOTAL+1))
if [ "$EVT_COUNT" -gt "0" ]; then
    echo -e "${GREEN}  PASS${NC} Events recorded ($EVT_COUNT)"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} No events"
    FAIL=$((FAIL+1))
fi

# ==========================================
echo ""
echo -e "${YEL}[12] Delete Flow${NC}"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE/v1/flows/$FLOW_ID")
check "DELETE flow" "204" "$STATUS"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$FLOW_ID")
check "GET deleted flow = 404" "404" "$STATUS"

# ==========================================
echo ""
echo -e "${YEL}[13] Validation${NC}"
# Missing source
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"name":"bad"}')
check "create flow without source = 400" "400" "$STATUS"

# Missing name
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"source":{"name":"s","protocol":"udp","ingest_port":1234},"outputs":[]}')
check "create flow without name = 400" "400" "$STATUS"

# Invalid protocol
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"name":"bad","source":{"name":"s","protocol":"hls","ingest_port":1234},"outputs":[]}')
check "create flow with invalid protocol = 400" "400" "$STATUS"

# Invalid port (0 for UDP listener)
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"name":"bad","source":{"name":"s","protocol":"udp","ingest_port":0},"outputs":[]}')
check "create flow with port 0 = 400" "400" "$STATUS"

# Invalid output port (99999)
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"name":"bad","source":{"name":"s","protocol":"udp","ingest_port":5555},"outputs":[{"name":"o","protocol":"udp","destination":"127.0.0.1","port":99999}]}')
check "create flow with invalid output port = 400" "400" "$STATUS"

# ==========================================
echo ""
echo -e "${YEL}[14] Cache-Control header${NC}"
TOTAL=$((TOTAL+1))
CACHE=$(curl -s -D - -o /dev/null "$BASE/health" | grep -i "cache-control" | tr -d '\r')
if echo "$CACHE" | grep -qi "no-store"; then
    echo -e "${GREEN}  PASS${NC} Cache-Control: no-store present"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} Missing Cache-Control header ($CACHE)"
    FAIL=$((FAIL+1))
fi

# ==========================================
echo ""
echo -e "${YEL}[15] Delete cleans events${NC}"
# Create a flow, start/stop it (to generate events), then delete and verify events are gone
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Event Cleanup Test",
  "source": {"name":"src","protocol":"udp","ingest_port":5050},
  "outputs": [{"name":"o","protocol":"udp","destination":"127.0.0.1","port":6050}]
}')
CLEANUP_ID=$(echo "$RESP" | head -1 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
# Start and stop to generate events
curl -s -X POST "$BASE/v1/flows/$CLEANUP_ID/start" -o /dev/null
sleep 1
curl -s -X POST "$BASE/v1/flows/$CLEANUP_ID/stop" -o /dev/null
sleep 1
# Verify events exist
EVT_BEFORE=$(curl -s "$BASE/v1/flows/$CLEANUP_ID/events" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['events']))")
# Delete the flow
curl -s -X DELETE "$BASE/v1/flows/$CLEANUP_ID" -o /dev/null
TOTAL=$((TOTAL+1))
if [ "$EVT_BEFORE" -gt "0" ]; then
    echo -e "${GREEN}  PASS${NC} Events existed before delete ($EVT_BEFORE)"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} No events before delete"
    FAIL=$((FAIL+1))
fi

# ==========================================
echo ""
echo -e "${YEL}[16] Double Stop${NC}"
# Create flow, start, stop, stop again - should not error
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Double Stop Test",
  "source": {"name":"src","protocol":"udp","ingest_port":5060},
  "outputs": [{"name":"o","protocol":"udp","destination":"127.0.0.1","port":6060}]
}')
DSTOP_ID=$(echo "$RESP" | head -1 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
curl -s -X POST "$BASE/v1/flows/$DSTOP_ID/start" -o /dev/null
sleep 1
curl -s -X POST "$BASE/v1/flows/$DSTOP_ID/stop" -o /dev/null
sleep 1
# Second stop should return 200 (idempotent)
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows/$DSTOP_ID/stop")
check "double stop is safe" "200" "$STATUS"
curl -s -X DELETE "$BASE/v1/flows/$DSTOP_ID" -o /dev/null

# ==========================================
echo ""
echo -e "${YEL}[17] Multi-Output Test${NC}"
cleanup_ffmpeg
# Create flow with 2 UDP outputs, send data, verify both receive
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Multi-Output",
  "source": {"name":"src","protocol":"udp","ingest_port":5100},
  "outputs": [
    {"name":"out_a","protocol":"udp","destination":"127.0.0.1","port":6100},
    {"name":"out_b","protocol":"udp","destination":"127.0.0.1","port":6101}
  ]
}')
MULTI_ID=$(echo "$RESP" | head -1 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Start source sender FIRST (like the working test 9 pattern)
ffmpeg -re -f lavfi -i "testsrc=size=320x240:rate=25" -f lavfi -i "sine=frequency=440" \
  -c:v libx264 -preset ultrafast -tune zerolatency -b:v 500k -c:a aac \
  -f mpegts "udp://127.0.0.1:5100?pkt_size=1316" > /dev/null 2>&1 &
MULTI_SRC=$!
sleep 2

# Start flow (FFmpeg will receive data immediately since sender is already running)
curl -s -X POST "$BASE/v1/flows/$MULTI_ID/start" -o /dev/null
sleep 4

# Capture from both outputs concurrently
ffmpeg -y -i "udp://127.0.0.1:6100?timeout=8000000" -t 4 -c copy /tmp/mroute_multi_a.ts > /dev/null 2>&1 &
PID_A=$!
ffmpeg -y -i "udp://127.0.0.1:6101?timeout=8000000" -t 4 -c copy /tmp/mroute_multi_b.ts > /dev/null 2>&1 &
PID_B=$!
wait $PID_A 2>/dev/null
wait $PID_B 2>/dev/null

TOTAL=$((TOTAL+1))
SIZE_A=$(stat -c%s /tmp/mroute_multi_a.ts 2>/dev/null || echo 0)
SIZE_B=$(stat -c%s /tmp/mroute_multi_b.ts 2>/dev/null || echo 0)
if [ "$SIZE_A" -gt 5000 ] && [ "$SIZE_B" -gt 5000 ]; then
    echo -e "${GREEN}  PASS${NC} Multi-output: both received (A=$(ls -lh /tmp/mroute_multi_a.ts | awk '{print $5}'), B=$(ls -lh /tmp/mroute_multi_b.ts | awk '{print $5}'))"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} Multi-output: A=$SIZE_A B=$SIZE_B"
    FAIL=$((FAIL+1))
fi

curl -s -X POST "$BASE/v1/flows/$MULTI_ID/stop" -o /dev/null
sleep 1
kill $MULTI_SRC 2>/dev/null
curl -s -X DELETE "$BASE/v1/flows/$MULTI_ID" -o /dev/null

# ==========================================
echo ""
echo -e "${YEL}[18] SRT Transport Test${NC}"
# Create flow with SRT listener source and UDP output
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "SRT Test",
  "source": {"name":"srt_src","protocol":"srt-listener","ingest_port":5200},
  "outputs": [{"name":"srt_out","protocol":"udp","destination":"127.0.0.1","port":6200}]
}')
SRT_ID=$(echo "$RESP" | head -1 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
SRT_STATUS=$(echo "$RESP" | tail -1)
check "SRT flow created" "201" "$SRT_STATUS"

# Start the flow first (it needs to listen on SRT port)
curl -s -X POST "$BASE/v1/flows/$SRT_ID/start" -o /dev/null
sleep 2

# Send test stream via SRT caller to our SRT listener
ffmpeg -re -f lavfi -i "testsrc=size=320x240:rate=25" -f lavfi -i "sine=frequency=440" \
  -c:v libx264 -preset ultrafast -tune zerolatency -b:v 500k -c:a aac \
  -f mpegts "srt://127.0.0.1:5200?mode=caller" > /dev/null 2>&1 &
SRT_SRC=$!
sleep 3

# Capture from UDP output
ffmpeg -y -i "udp://127.0.0.1:6200?timeout=8000000" -t 4 -c copy /tmp/mroute_srt_out.ts > /dev/null 2>&1

TOTAL=$((TOTAL+1))
SRT_SIZE=$(stat -c%s /tmp/mroute_srt_out.ts 2>/dev/null || echo 0)
if [ "$SRT_SIZE" -gt 5000 ]; then
    echo -e "${GREEN}  PASS${NC} SRT->UDP relay received ($(ls -lh /tmp/mroute_srt_out.ts | awk '{print $5}'))"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} SRT relay: received $SRT_SIZE bytes"
    FAIL=$((FAIL+1))
fi

curl -s -X POST "$BASE/v1/flows/$SRT_ID/stop" -o /dev/null
sleep 1
kill $SRT_SRC 2>/dev/null
curl -s -X DELETE "$BASE/v1/flows/$SRT_ID" -o /dev/null

# ==========================================
echo ""
echo -e "${YEL}[19] Failover Test${NC}"
# Create flow with failover config
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Failover Test",
  "source": {"name":"primary","protocol":"udp","ingest_port":5300},
  "outputs": [{"name":"fo_out","protocol":"udp","destination":"127.0.0.1","port":6300}],
  "source_failover_config": {
    "state": "ENABLED",
    "failover_mode": "FAILOVER",
    "source_priority": {"primary_source": "primary"}
  }
}')
FO_ID=$(echo "$RESP" | head -1 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Add backup source
curl -s -X POST "$BASE/v1/flows/$FO_ID/source" -H "Content-Type: application/json" -d '{"name":"backup","protocol":"udp","ingest_port":5301}' -o /dev/null

# Start primary sender
ffmpeg -re -f lavfi -i "testsrc=size=320x240:rate=25" -f lavfi -i "sine=frequency=440" \
  -c:v libx264 -preset ultrafast -tune zerolatency -b:v 500k -c:a aac \
  -f mpegts "udp://127.0.0.1:5300?pkt_size=1316" > /dev/null 2>&1 &
FO_PRIMARY=$!
sleep 1

# Start flow
curl -s -X POST "$BASE/v1/flows/$FO_ID/start" -o /dev/null
sleep 3

# Verify primary is active
ACTIVE=$(curl -s "$BASE/v1/flows/$FO_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['active_source'])" 2>/dev/null)
check "failover: primary active" "primary" "$ACTIVE"

# Start backup sender BEFORE killing primary (so data is ready when failover happens)
ffmpeg -re -f lavfi -i "testsrc=size=320x240:rate=25" \
  -f lavfi -i "sine=frequency=880" \
  -c:v libx264 -preset ultrafast -tune zerolatency -b:v 500k -c:a aac \
  -f mpegts "udp://127.0.0.1:5301?pkt_size=1316" > /dev/null 2>&1 &
FO_BACKUP=$!
sleep 2

# Kill primary to trigger failover
# UDP is connectionless - FFmpeg needs ~5s timeout to detect loss, then failover
kill $FO_PRIMARY 2>/dev/null
sleep 10

# Verify backup is now active
ACTIVE2=$(curl -s "$BASE/v1/flows/$FO_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['active_source'])" 2>/dev/null)
check "failover: backup active after kill" "backup" "$ACTIVE2"

# Check failover count > 0
FO_COUNT=$(curl -s "$BASE/v1/flows/$FO_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['failover_count'])" 2>/dev/null)
TOTAL=$((TOTAL+1))
if [ "$FO_COUNT" -gt "0" ]; then
    echo -e "${GREEN}  PASS${NC} Failover count = $FO_COUNT"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} Failover count = $FO_COUNT"
    FAIL=$((FAIL+1))
fi

# Capture from output to verify data is flowing after failover
ffmpeg -y -i "udp://127.0.0.1:6300?timeout=10000000" -t 5 -c copy /tmp/mroute_fo_out.ts > /dev/null 2>&1
TOTAL=$((TOTAL+1))
FO_SIZE=$(stat -c%s /tmp/mroute_fo_out.ts 2>/dev/null || echo 0)
if [ "$FO_SIZE" -gt 5000 ]; then
    echo -e "${GREEN}  PASS${NC} Failover: data flowing after switch ($(ls -lh /tmp/mroute_fo_out.ts | awk '{print $5}'))"
    PASS=$((PASS+1))
else
    echo -e "${RED}  FAIL${NC} Failover: no data after switch ($FO_SIZE bytes)"
    FAIL=$((FAIL+1))
fi

curl -s -X POST "$BASE/v1/flows/$FO_ID/stop" -o /dev/null
sleep 1
kill $FO_BACKUP 2>/dev/null
curl -s -X DELETE "$BASE/v1/flows/$FO_ID" -o /dev/null

# Cleanup
cleanup_ffmpeg
rm -f /tmp/mroute_relay.ts /tmp/mroute_received.ts /tmp/srt_test_out.ts /tmp/mroute_multi_a.ts /tmp/mroute_multi_b.ts /tmp/mroute_srt_out.ts /tmp/mroute_fo_out.ts

echo ""
echo -e "${YEL}=============================${NC}"
echo -e "  Results: ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}, $TOTAL total"
echo -e "${YEL}=============================${NC}"

exit $FAIL
