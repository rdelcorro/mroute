#!/usr/bin/env bash
# ============================================================================
# mroute Integration Test Suite
# Tests all features with REAL media streams and validates outputs with ffprobe
# Compatible with Linux and macOS
# ============================================================================

set -o pipefail
PASS=0; FAIL=0; TOTAL=0; SKIP=0
GREEN='\033[0;32m'; RED='\033[0;31m'; YEL='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_PORT=18199
BASE="http://127.0.0.1:${TEST_PORT}"
BG_PIDS=()
SERVER_PID=""
TEST_TMPDIR=""

# Kill any leftover processes from previous test runs
# Use a lockfile to prevent concurrent test runs
LOCKFILE="/tmp/.mroute_test_lock"
if [ -f "$LOCKFILE" ]; then
    OLD_PID=$(cat "$LOCKFILE" 2>/dev/null)
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
        kill -9 "$OLD_PID" 2>/dev/null || true
    fi
    rm -f "$LOCKFILE"
fi
echo $$ > "$LOCKFILE"
# Kill stale server processes
pkill -9 -f "mroute_test\..*/mroute" 2>/dev/null || true
# Kill ALL stale ffmpeg processes (test sources AND engine-spawned)
pkill -9 -f "ffmpeg.*testsrc" 2>/dev/null || true
pkill -9 -f "ffmpeg.*sine" 2>/dev/null || true
pkill -9 -f "ffmpeg.*udp://127.0.0.1" 2>/dev/null || true
# Wait for ports to be released
sleep 2
# Also check if the test port is in use and kill that process
if command -v lsof >/dev/null 2>&1 && lsof -ti :${TEST_PORT} >/dev/null 2>&1; then
    kill -9 $(lsof -ti :${TEST_PORT}) 2>/dev/null || true
    sleep 1
fi
rm -rf /tmp/mroute_test.* 2>/dev/null || true

# ============================================================================
# Helpers
# ============================================================================

file_size() {
    wc -c < "$1" 2>/dev/null | tr -d ' '
}

check() {
    TOTAL=$((TOTAL+1))
    if [ "$2" = "$3" ]; then
        echo -e "  ${GREEN}PASS${NC} $1"
        PASS=$((PASS+1))
    else
        echo -e "  ${RED}FAIL${NC} $1 (expected='$2' got='$3')"
        FAIL=$((FAIL+1))
    fi
}

skip() {
    TOTAL=$((TOTAL+1))
    SKIP=$((SKIP+1))
    echo -e "  ${YEL}SKIP${NC} $1"
}

# Validate a captured media file with ffprobe
# Usage: validate_media <file> [min_size_bytes]
# Prints: OK:h264/aac@320x240(123456b) or FAIL:reason
validate_media() {
    local file="$1"
    local min_size="${2:-5000}"

    if [ ! -f "$file" ]; then
        echo "FAIL:file_not_found"
        return 1
    fi

    local size
    size=$(file_size "$file")
    if [ "$size" -lt "$min_size" ]; then
        echo "FAIL:too_small(${size}b)"
        return 1
    fi

    local probe
    probe=$(ffprobe -v quiet -print_format json -show_streams -show_format "$file" 2>/dev/null)
    if [ -z "$probe" ]; then
        echo "FAIL:ffprobe_error"
        return 1
    fi

    local info
    info=$(echo "$probe" | python3 -c "
import sys, json
d = json.load(sys.stdin)
streams = d.get('streams', [])
vs = [s for s in streams if s.get('codec_type') == 'video']
a_s = [s for s in streams if s.get('codec_type') == 'audio']
vc = vs[0]['codec_name'] if vs else 'none'
ac = a_s[0]['codec_name'] if a_s else 'none'
vr = f\"{vs[0].get('width','?')}x{vs[0].get('height','?')}\" if vs else 'none'
print(f'{len(vs)} {len(a_s)} {vc} {ac} {vr}')
" 2>/dev/null)

    if [ -z "$info" ]; then
        echo "FAIL:parse_error"
        return 1
    fi

    local vcount acount vcodec acodec vres
    read -r vcount acount vcodec acodec vres <<< "$info"

    if [ "$vcount" -lt 1 ]; then
        echo "FAIL:no_video_stream"
        return 1
    fi
    if [ "$acount" -lt 1 ]; then
        echo "FAIL:no_audio_stream"
        return 1
    fi

    echo "OK:${vcodec}/${acodec}@${vres}(${size}b)"
    return 0
}

# Check a media validation result
check_media() {
    local label="$1"
    local file="$2"
    local min_size="${3:-5000}"
    TOTAL=$((TOTAL+1))
    local result
    result=$(validate_media "$file" "$min_size")
    if [ $? -eq 0 ]; then
        echo -e "  ${GREEN}PASS${NC} $label [$result]"
        PASS=$((PASS+1))
    else
        echo -e "  ${RED}FAIL${NC} $label [$result]"
        FAIL=$((FAIL+1))
    fi
}

# Start a background process and track its PID
bg_start() {
    "$@" </dev/null >/dev/null 2>&1 &
    local pid=$!
    BG_PIDS+=("$pid")
    echo "$pid"
}

# Kill all tracked background processes
bg_killall() {
    for pid in "${BG_PIDS[@]}"; do
        kill "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    BG_PIDS=()
}

# Kill specific background PIDs
bg_kill() {
    for pid in "$@"; do
        kill "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
        # Remove from tracking array
        local new_pids=()
        for p in "${BG_PIDS[@]}"; do
            [ "$p" != "$pid" ] && new_pids+=("$p")
        done
        BG_PIDS=("${new_pids[@]}")
    done
}

cleanup() {
    echo ""
    echo -e "${YEL}Cleaning up...${NC}"
    bg_killall
    if [ -n "$SERVER_PID" ]; then
        kill "$SERVER_PID" 2>/dev/null
        wait "$SERVER_PID" 2>/dev/null
    fi
    # Kill any FFmpeg processes spawned by the engine or test
    pkill -9 -f "ffmpeg.*testsrc" 2>/dev/null || true
    pkill -9 -f "ffmpeg.*sine" 2>/dev/null || true
    pkill -9 -f "ffmpeg.*udp://127.0.0.1" 2>/dev/null || true
    sleep 1
    if [ -n "$TEST_TMPDIR" ] && [ -d "$TEST_TMPDIR" ]; then
        if [ "$FAIL" -gt 0 ] 2>/dev/null; then
            echo "  Preserving temp dir for debugging: $TEST_TMPDIR"
        else
            rm -rf "$TEST_TMPDIR"
        fi
    fi
    rm -f "$LOCKFILE" 2>/dev/null
}

trap cleanup EXIT INT TERM

wait_for_server() {
    local max_wait=30
    for i in $(seq 1 $max_wait); do
        if curl -sf -o /dev/null "$BASE/health" 2>/dev/null; then
            return 0
        fi
        sleep 0.5
    done
    echo -e "${RED}FATAL: Server did not start within ${max_wait}s${NC}"
    if [ -f "$TEST_TMPDIR/server.log" ]; then
        echo "Server log:"
        tail -20 "$TEST_TMPDIR/server.log"
    fi
    exit 1
}

# Generate test media source (testsrc + sine → H.264/AAC → mpegts)
# Usage: start_udp_source <dest_port> [bitrate] [frequency]
start_udp_source() {
    local port="$1"
    local bitrate="${2:-500k}"
    local freq="${3:-440}"
    bg_start ffmpeg -hide_banner -loglevel error \
        -re -f lavfi -i "testsrc=size=320x240:rate=25" \
        -f lavfi -i "sine=frequency=${freq}:sample_rate=44100" \
        -c:v libx264 -preset ultrafast -tune zerolatency -g 25 -b:v "$bitrate" \
        -c:a aac -b:a 128k \
        -f mpegts "udp://127.0.0.1:${port}?pkt_size=1316"
}

# Capture media from UDP output
# Usage: capture_udp <port> <output_file> [duration] [timeout_us]
capture_udp() {
    local port="$1"
    local outfile="$2"
    local duration="${3:-5}"
    local udp_timeout="${4:-10000000}"
    local wall_timeout=$((duration + 15))
    timeout "$wall_timeout" ffmpeg -hide_banner -loglevel error \
        -y -f mpegts -fflags +genpts+discardcorrupt \
        -i "udp://127.0.0.1:${port}?timeout=${udp_timeout}&overrun_nonfatal=1&fifo_size=131072" \
        -t "$duration" -c copy "$outfile" > /dev/null 2>&1 || true
}

# ============================================================================
# Prerequisites
# ============================================================================

echo -e "${YEL}============================================${NC}"
echo -e "${YEL}  mroute Integration Tests (Real Media)${NC}"
echo -e "${YEL}============================================${NC}"
echo ""

# Verify previous cleanup succeeded
echo -e "${BLUE}[Setup] Cleaning up stale processes...${NC}"
if curl -sf -o /dev/null "http://127.0.0.1:${TEST_PORT}/health" 2>/dev/null; then
    # Force-kill anything on the test port
    if command -v lsof >/dev/null 2>&1; then
        kill -9 $(lsof -ti :${TEST_PORT}) 2>/dev/null || true
    elif command -v fuser >/dev/null 2>&1; then
        fuser -k ${TEST_PORT}/tcp 2>/dev/null || true
    fi
    sleep 1
fi
if curl -sf -o /dev/null "http://127.0.0.1:${TEST_PORT}/health" 2>/dev/null; then
    echo -e "${RED}FATAL: Port ${TEST_PORT} still in use after cleanup${NC}"
    exit 1
fi
echo "  Clean"

echo -e "${BLUE}[Setup] Checking prerequisites...${NC}"
MISSING=""
for cmd in ffmpeg ffprobe curl python3 go; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        MISSING="$MISSING $cmd"
    fi
done
if [ -n "$MISSING" ]; then
    echo -e "${RED}FATAL: Missing required tools:${MISSING}${NC}"
    exit 1
fi
echo "  Tools: OK"

# Check SRT support
HAS_SRT=false
if ffmpeg -protocols 2>/dev/null | grep -q "  srt"; then
    HAS_SRT=true
    echo "  SRT: available"
else
    echo -e "  SRT: ${YEL}not available (SRT tests will be skipped)${NC}"
fi

# ============================================================================
# Build & Start
# ============================================================================

echo ""
echo -e "${BLUE}[Setup] Building mroute...${NC}"
TEST_TMPDIR=$(mktemp -d "${TMPDIR:-/tmp}/mroute_test.XXXXXX")

cd "$SCRIPT_DIR"
if ! go build -o "$TEST_TMPDIR/mroute" ./cmd/mroute/ 2>&1; then
    echo -e "${RED}FATAL: Build failed${NC}"
    exit 1
fi
echo "  Binary: $TEST_TMPDIR/mroute"

# Create test config
cat > "$TEST_TMPDIR/config.yaml" <<EOFCFG
api:
  host: "127.0.0.1"
  port: ${TEST_PORT}
storage:
  dsn: "sqlite:${TEST_TMPDIR}/test.db"
ffmpeg:
  path: "ffmpeg"
  probe_path: "ffprobe"
  max_flows: 20
EOFCFG

echo -e "${BLUE}[Setup] Starting server on port ${TEST_PORT}...${NC}"
"$TEST_TMPDIR/mroute" -config "$TEST_TMPDIR/config.yaml" > "$TEST_TMPDIR/server.log" 2>&1 &
SERVER_PID=$!
wait_for_server
echo "  Server PID: $SERVER_PID"
echo ""

# ============================================================================
# A. API & CRUD Tests
# ============================================================================

echo -e "${YEL}[A.1] Health${NC}"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/health")
check "GET /health" "200" "$STATUS"

echo ""
echo -e "${YEL}[A.2] Create Flow${NC}"
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
BODY=$(echo "$RESP" | sed '$d')
check "POST /v1/flows status" "201" "$STATUS"
FLOW_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
FLOW_STATUS=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")
check "flow status=STANDBY" "STANDBY" "$FLOW_STATUS"

echo ""
echo -e "${YEL}[A.3] Get Flow${NC}"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$FLOW_ID")
check "GET /v1/flows/:id" "200" "$STATUS"

echo ""
echo -e "${YEL}[A.4] List Flows${NC}"
COUNT=$(curl -s "$BASE/v1/flows" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['flows']))")
check "flow count" "1" "$COUNT"

echo ""
echo -e "${YEL}[A.5] Add Output${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$FLOW_ID/outputs" -H "Content-Type: application/json" -d '{
  "name": "out2",
  "protocol": "udp",
  "destination": "127.0.0.1",
  "port": 6001
}')
STATUS=$(echo "$RESP" | tail -1)
check "POST add output" "200" "$STATUS"
OUT_COUNT=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(len(json.load(sys.stdin)['outputs']))")
check "outputs count=2" "2" "$OUT_COUNT"

echo ""
echo -e "${YEL}[A.6] Remove Output${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X DELETE "$BASE/v1/flows/$FLOW_ID/outputs/out2")
STATUS=$(echo "$RESP" | tail -1)
check "DELETE remove output" "200" "$STATUS"
OUT_COUNT=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(len(json.load(sys.stdin)['outputs']))")
check "outputs count=1" "1" "$OUT_COUNT"

echo ""
echo -e "${YEL}[A.7] Add Source (failover)${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$FLOW_ID/source" -H "Content-Type: application/json" -d '{
  "name": "backup",
  "protocol": "udp",
  "ingest_port": 5001
}')
STATUS=$(echo "$RESP" | tail -1)
check "POST add backup source" "200" "$STATUS"
SRC_COUNT=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; f=json.load(sys.stdin); print(len([f.get('source')]+f.get('sources',[])))")
check "sources count=2" "2" "$SRC_COUNT"

echo ""
echo -e "${YEL}[A.8] Remove Source${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X DELETE "$BASE/v1/flows/$FLOW_ID/source/backup")
STATUS=$(echo "$RESP" | tail -1)
check "DELETE remove source" "200" "$STATUS"

# ============================================================================
# B. Validation Tests
# ============================================================================

echo ""
echo -e "${YEL}[B] Validation${NC}"

STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"name":"bad"}')
check "missing source = 400" "400" "$STATUS"

STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"source":{"name":"s","protocol":"udp","ingest_port":1234},"outputs":[]}')
check "missing name = 400" "400" "$STATUS"

STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"name":"bad","source":{"name":"s","protocol":"hls","ingest_port":1234},"outputs":[]}')
check "invalid protocol = 400" "400" "$STATUS"

STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"name":"bad","source":{"name":"s","protocol":"udp","ingest_port":0},"outputs":[]}')
check "port 0 = 400" "400" "$STATUS"

STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{"name":"bad","source":{"name":"s","protocol":"udp","ingest_port":5555},"outputs":[{"name":"o","protocol":"udp","destination":"127.0.0.1","port":99999}]}')
check "invalid output port = 400" "400" "$STATUS"

# ============================================================================
# C. Live Transport: UDP -> UDP (with real media validation)
# ============================================================================

echo ""
echo -e "${YEL}[C.1] Live UDP Transport (real media validation)${NC}"

# Start source FIRST (sends test pattern to port 5000)
SRC_PID=$(start_udp_source 5000 1000k)
sleep 2

# Start the flow
curl -s -m 10 -X POST "$BASE/v1/flows/$FLOW_ID/start" -o /dev/null
sleep 4

# Capture output and validate
capture_udp 6000 "$TEST_TMPDIR/udp_relay.ts" 5
check_media "UDP relay: valid H.264/AAC output" "$TEST_TMPDIR/udp_relay.ts"

# Check active source metric
ACTIVE_SRC=$(curl -s "$BASE/v1/flows/$FLOW_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['active_source'])" 2>/dev/null)
check "active_source = primary" "primary" "$ACTIVE_SRC"

echo ""
echo -e "${YEL}[C.2] Stop Flow${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$FLOW_ID/stop")
STATUS=$(echo "$RESP" | tail -1)
check "POST stop flow" "200" "$STATUS"
FLOW_STATUS=$(curl -s "$BASE/v1/flows/$FLOW_ID" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")
check "flow status=STANDBY" "STANDBY" "$FLOW_STATUS"
bg_kill "$SRC_PID"
sleep 1

echo ""
echo -e "${YEL}[C.3] Events Recorded${NC}"
EVT_COUNT=$(curl -s "$BASE/v1/flows/$FLOW_ID/events" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['events']))")
TOTAL=$((TOTAL+1))
if [ "$EVT_COUNT" -gt "0" ]; then
    echo -e "  ${GREEN}PASS${NC} Events recorded ($EVT_COUNT)"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} No events"
    FAIL=$((FAIL+1))
fi

echo ""
echo -e "${YEL}[C.4] Delete Flow${NC}"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE/v1/flows/$FLOW_ID")
check "DELETE flow" "204" "$STATUS"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$FLOW_ID")
check "GET deleted flow = 404" "404" "$STATUS"

# ============================================================================
# D. Multi-Output Fan-out (real media validation)
# ============================================================================

echo ""
echo -e "${YEL}[D] Multi-Output Fan-out${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Multi-Output",
  "source": {"name":"src","protocol":"udp","ingest_port":5100},
  "outputs": [
    {"name":"out_a","protocol":"udp","destination":"127.0.0.1","port":6100},
    {"name":"out_b","protocol":"udp","destination":"127.0.0.1","port":6101}
  ]
}')
MULTI_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

SRC_PID=$(start_udp_source 5100)
sleep 2

curl -s -X POST "$BASE/v1/flows/$MULTI_ID/start" -o /dev/null
sleep 4

# Capture from both outputs concurrently
capture_udp 6100 "$TEST_TMPDIR/multi_a.ts" 4 &
PID_A=$!
capture_udp 6101 "$TEST_TMPDIR/multi_b.ts" 4 &
PID_B=$!
wait $PID_A 2>/dev/null
wait $PID_B 2>/dev/null

check_media "Output A: valid H.264/AAC" "$TEST_TMPDIR/multi_a.ts"
check_media "Output B: valid H.264/AAC" "$TEST_TMPDIR/multi_b.ts"

curl -s -X POST "$BASE/v1/flows/$MULTI_ID/stop" -o /dev/null
sleep 1
bg_kill "$SRC_PID"
curl -s -X DELETE "$BASE/v1/flows/$MULTI_ID" -o /dev/null

# ============================================================================
# D.2 Hot Output Add (add output while flow is running)
# ============================================================================

echo ""
echo -e "${YEL}[D.2] Hot Output Add (add while running)${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Hot Output Add",
  "source": {"name":"src","protocol":"udp","ingest_port":5150},
  "outputs": [
    {"name":"original","protocol":"udp","destination":"127.0.0.1","port":6150}
  ]
}')
HOT_OUT_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

HOT_SRC=$(start_udp_source 5150)
sleep 2

# Start flow with only 1 output
curl -s -X POST "$BASE/v1/flows/$HOT_OUT_ID/start" -o /dev/null
sleep 5

# Verify original output receives data
capture_udp 6150 "$TEST_TMPDIR/hot_before.ts" 4
check_media "hot-add: original output working" "$TEST_TMPDIR/hot_before.ts" 3000

# CONTINUITY TEST: Start capturing from original output BEFORE adding the new output.
# This capture runs THROUGH the add operation to prove the existing output is not interrupted.
capture_udp 6150 "$TEST_TMPDIR/hot_continuity.ts" 10 &
PID_CONT=$!

sleep 1

# Add a second output while flow is running (hitless - no restart needed)
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$HOT_OUT_ID/outputs" -H "Content-Type: application/json" -d '{
  "name": "hot_added",
  "protocol": "udp",
  "destination": "127.0.0.1",
  "port": 6151
}')
check "hot-add output API" "200" "$(echo "$RESP" | tail -1)"

# Wait for the new output FFmpeg to start, then verify both outputs
sleep 4

# Wait for continuity capture to finish
wait $PID_CONT 2>/dev/null

# Validate the continuity capture - should have ~10s of uninterrupted media
check_media "hot-add: original output uninterrupted during add" "$TEST_TMPDIR/hot_continuity.ts" 50000

# Verify BOTH outputs now receive data
capture_udp 6150 "$TEST_TMPDIR/hot_orig.ts" 4 &
PID_HO1=$!
capture_udp 6151 "$TEST_TMPDIR/hot_new.ts" 4 &
PID_HO2=$!
wait $PID_HO1 2>/dev/null
wait $PID_HO2 2>/dev/null

check_media "hot-add: original output still working" "$TEST_TMPDIR/hot_orig.ts"
check_media "hot-add: new output receives data" "$TEST_TMPDIR/hot_new.ts"

curl -s -X POST "$BASE/v1/flows/$HOT_OUT_ID/stop" -o /dev/null
sleep 1
bg_kill "$HOT_SRC"
curl -s -X DELETE "$BASE/v1/flows/$HOT_OUT_ID" -o /dev/null

# ============================================================================
# D.3 Hot Output Remove (remove output while flow is running)
# ============================================================================

echo ""
echo -e "${YEL}[D.3] Hot Output Remove (remove while running)${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Hot Output Remove",
  "source": {"name":"src","protocol":"udp","ingest_port":5160},
  "outputs": [
    {"name":"keep","protocol":"udp","destination":"127.0.0.1","port":6160},
    {"name":"remove_me","protocol":"udp","destination":"127.0.0.1","port":6161}
  ]
}')
HOT_RM_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

HOT_RM_SRC=$(start_udp_source 5160)
sleep 2

curl -s -X POST "$BASE/v1/flows/$HOT_RM_ID/start" -o /dev/null
sleep 3

# Verify both outputs work before removal
capture_udp 6160 "$TEST_TMPDIR/hot_rm_keep_before.ts" 3 &
PID_KP=$!
capture_udp 6161 "$TEST_TMPDIR/hot_rm_gone_before.ts" 3 &
PID_GP=$!
wait $PID_KP 2>/dev/null
wait $PID_GP 2>/dev/null
check_media "hot-rm: both outputs before remove" "$TEST_TMPDIR/hot_rm_keep_before.ts" 3000

# CONTINUITY TEST: Start capturing from the "keep" output BEFORE removing the other output.
# This proves the remaining output is not interrupted by the remove operation.
capture_udp 6160 "$TEST_TMPDIR/hot_rm_continuity.ts" 10 &
PID_RM_CONT=$!

sleep 1

# Remove one output while running (hitless - no restart needed)
RESP=$(curl -s -w "\n%{http_code}" -X DELETE "$BASE/v1/flows/$HOT_RM_ID/outputs/remove_me")
check "hot-rm output API" "200" "$(echo "$RESP" | tail -1)"

# Wait for continuity capture to finish
wait $PID_RM_CONT 2>/dev/null

# Validate the continuity capture - should have ~10s of uninterrupted media
check_media "hot-rm: remaining output uninterrupted during remove" "$TEST_TMPDIR/hot_rm_continuity.ts" 50000

# Brief wait then verify remaining output still works
sleep 1
capture_udp 6160 "$TEST_TMPDIR/hot_rm_keep_after.ts" 4
check_media "hot-rm: remaining output after remove" "$TEST_TMPDIR/hot_rm_keep_after.ts"

# Verify removed output gets nothing (capture should timeout with tiny/no file)
capture_udp 6161 "$TEST_TMPDIR/hot_rm_gone_after.ts" 3 6000000
TOTAL=$((TOTAL+1))
GONE_SIZE=$(file_size "$TEST_TMPDIR/hot_rm_gone_after.ts" 2>/dev/null || echo 0)
if [ "${GONE_SIZE:-0}" -lt 1000 ]; then
    echo -e "  ${GREEN}PASS${NC} hot-rm: removed output no longer receives (${GONE_SIZE}b)"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} hot-rm: removed output still receiving (${GONE_SIZE}b)"
    FAIL=$((FAIL+1))
fi

curl -s -X POST "$BASE/v1/flows/$HOT_RM_ID/stop" -o /dev/null
sleep 1
bg_kill "$HOT_RM_SRC"
curl -s -X DELETE "$BASE/v1/flows/$HOT_RM_ID" -o /dev/null

# ============================================================================
# D.4 Hot Source Add (add failover source while running)
# ============================================================================

echo ""
echo -e "${YEL}[D.4] Hot Source Add (add failover source while running)${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Hot Source Add",
  "source": {"name":"primary","protocol":"udp","ingest_port":5170},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6170}],
  "source_failover_config": {
    "state": "ENABLED",
    "failover_mode": "FAILOVER",
    "source_priority": {"primary_source": "primary"}
  }
}')
HOT_SRC_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

HOT_SRC_PRI=$(start_udp_source 5170 500k 440)
sleep 2

curl -s -X POST "$BASE/v1/flows/$HOT_SRC_ID/start" -o /dev/null
sleep 3

# Verify primary works
capture_udp 6170 "$TEST_TMPDIR/hot_src_pri.ts" 3
check_media "hot-src: primary running" "$TEST_TMPDIR/hot_src_pri.ts" 3000

# Start backup sender on a port we'll configure next
HOT_SRC_BAK=$(start_udp_source 5171 500k 880)
sleep 1

# Add backup source while running (requires input restart, outputs stay running)
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$HOT_SRC_ID/source" -H "Content-Type: application/json" \
  -d '{"name":"backup","protocol":"udp","ingest_port":5171}')
check "hot-src add source API" "200" "$(echo "$RESP" | tail -1)"

sleep 4

# Kill primary to trigger failover to the hot-added backup
bg_kill "$HOT_SRC_PRI"
echo "  ... waiting for failover to hot-added source..."
sleep 15

# Verify backup is now active
ACTIVE_HOT=$(curl -s "$BASE/v1/flows/$HOT_SRC_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['active_source'])" 2>/dev/null)
check "hot-src: failover to hot-added backup" "backup" "$ACTIVE_HOT"

# Capture after failover
capture_udp 6170 "$TEST_TMPDIR/hot_src_bak.ts" 5
check_media "hot-src: media after failover to hot-added" "$TEST_TMPDIR/hot_src_bak.ts"

curl -s -X POST "$BASE/v1/flows/$HOT_SRC_ID/stop" -o /dev/null
sleep 1
bg_kill "$HOT_SRC_BAK"
curl -s -X DELETE "$BASE/v1/flows/$HOT_SRC_ID" -o /dev/null

# ============================================================================
# D.5 Hot Source Remove (remove failover source while running)
# ============================================================================

echo ""
echo -e "${YEL}[D.5] Hot Source Remove (remove while running)${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Hot Source Remove",
  "source": {"name":"primary","protocol":"udp","ingest_port":5180},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6180}],
  "source_failover_config": {
    "state": "ENABLED",
    "failover_mode": "FAILOVER",
    "source_priority": {"primary_source": "primary"}
  }
}')
HOT_SRM_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Add backup source (while stopped, so flow has 2 sources)
curl -s -X POST "$BASE/v1/flows/$HOT_SRM_ID/source" -H "Content-Type: application/json" \
  -d '{"name":"backup","protocol":"udp","ingest_port":5181}' -o /dev/null

# Start primary sender
HOT_SRM_PRI=$(start_udp_source 5180 500k 440)
sleep 2

# Start flow with 2 sources
curl -s -X POST "$BASE/v1/flows/$HOT_SRM_ID/start" -o /dev/null
sleep 4

# Verify flow is running with media
capture_udp 6180 "$TEST_TMPDIR/hot_srm_before.ts" 3
check_media "hot-src-rm: media before source remove" "$TEST_TMPDIR/hot_srm_before.ts" 3000

# Remove backup source while flow is running (triggers restartInputIfRunning)
RESP=$(curl -s -w "\n%{http_code}" -X DELETE "$BASE/v1/flows/$HOT_SRM_ID/source/backup")
check "hot-src-rm: remove source API" "200" "$(echo "$RESP" | tail -1)"

# Wait for restart to complete
sleep 5

# Verify flow is still running on primary with good media
FLOW_ST=$(curl -s "$BASE/v1/flows/$HOT_SRM_ID" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])" 2>/dev/null)
check "hot-src-rm: flow still ACTIVE after remove" "ACTIVE" "$FLOW_ST"

capture_udp 6180 "$TEST_TMPDIR/hot_srm_after.ts" 4
check_media "hot-src-rm: media after source remove" "$TEST_TMPDIR/hot_srm_after.ts" 20000

curl -s -X POST "$BASE/v1/flows/$HOT_SRM_ID/stop" -o /dev/null
sleep 1
bg_kill "$HOT_SRM_PRI"
curl -s -X DELETE "$BASE/v1/flows/$HOT_SRM_ID" -o /dev/null

# ============================================================================
# D.6 Remove PRIMARY Source While Running
# ============================================================================

echo ""
echo -e "${YEL}[D.6] Remove PRIMARY Source While Running${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Remove Primary Source",
  "source": {"name":"primary","protocol":"udp","ingest_port":5190},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6190}],
  "source_failover_config": {
    "state": "ENABLED",
    "failover_mode": "FAILOVER",
    "source_priority": {"primary_source": "primary"}
  }
}')
RM_PRI_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Add backup source (while stopped)
curl -s -X POST "$BASE/v1/flows/$RM_PRI_ID/source" -H "Content-Type: application/json" \
  -d '{"name":"backup","protocol":"udp","ingest_port":5191}' -o /dev/null

# Start BOTH source generators
RM_PRI_SRC1=$(start_udp_source 5190 500k 440)
RM_PRI_SRC2=$(start_udp_source 5191 500k 880)
sleep 2

# Start flow (primary is active)
curl -s -X POST "$BASE/v1/flows/$RM_PRI_ID/start" -o /dev/null
sleep 4

# Verify primary is active and media flows
ACTIVE_BEFORE=$(curl -s "$BASE/v1/flows/$RM_PRI_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['active_source'])" 2>/dev/null)
check "rm-primary: primary is active" "primary" "$ACTIVE_BEFORE"

capture_udp 6190 "$TEST_TMPDIR/rm_pri_before.ts" 3
check_media "rm-primary: media before removal" "$TEST_TMPDIR/rm_pri_before.ts" 3000

# Remove the PRIMARY source while running (backup should become new primary)
RESP=$(curl -s -w "\n%{http_code}" -X DELETE "$BASE/v1/flows/$RM_PRI_ID/source/primary")
check "rm-primary: remove primary API" "200" "$(echo "$RESP" | tail -1)"

# Wait for restart to complete
sleep 6

# Verify flow is still ACTIVE with the backup as the sole source
FLOW_ST=$(curl -s "$BASE/v1/flows/$RM_PRI_ID" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])" 2>/dev/null)
check "rm-primary: flow still ACTIVE" "ACTIVE" "$FLOW_ST"

# Verify media still flows from the backup (now promoted to primary)
capture_udp 6190 "$TEST_TMPDIR/rm_pri_after.ts" 4
check_media "rm-primary: media after primary removal" "$TEST_TMPDIR/rm_pri_after.ts" 20000

curl -s -X POST "$BASE/v1/flows/$RM_PRI_ID/stop" -o /dev/null
sleep 1
bg_kill "$RM_PRI_SRC1" "$RM_PRI_SRC2"
curl -s -X DELETE "$BASE/v1/flows/$RM_PRI_ID" -o /dev/null

# ============================================================================
# D.7 UpdateOutput Hot-Swap While Running (change port)
# ============================================================================

echo ""
echo -e "${YEL}[D.7] UpdateOutput Hot-Swap (change port while running)${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Hot Swap Output",
  "source": {"name":"src","protocol":"udp","ingest_port":5210},
  "outputs": [
    {"name":"swappable","protocol":"udp","destination":"127.0.0.1","port":6210}
  ]
}')
SWAP_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

SWAP_SRC=$(start_udp_source 5210)
sleep 2

curl -s -X POST "$BASE/v1/flows/$SWAP_ID/start" -o /dev/null
sleep 4

# Verify original port works
capture_udp 6210 "$TEST_TMPDIR/swap_before.ts" 3
check_media "hot-swap: media on original port 6210" "$TEST_TMPDIR/swap_before.ts" 3000

# Update output to different port (hot-swap: remove old + add new)
RESP=$(curl -s -w "\n%{http_code}" -X PUT "$BASE/v1/flows/$SWAP_ID/outputs/swappable" \
  -H "Content-Type: application/json" -d '{"port": 6211}')
check "hot-swap: update output API" "200" "$(echo "$RESP" | tail -1)"

# Verify updated port in response
UPDATED_PORT=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; f=json.load(sys.stdin); print([o['port'] for o in f['outputs'] if o['name']=='swappable'][0])" 2>/dev/null)
check "hot-swap: port updated to 6211" "6211" "$UPDATED_PORT"

sleep 3

# Verify media on NEW port
capture_udp 6211 "$TEST_TMPDIR/swap_after.ts" 4
check_media "hot-swap: media on new port 6211" "$TEST_TMPDIR/swap_after.ts"

# Verify OLD port no longer receives
capture_udp 6210 "$TEST_TMPDIR/swap_old.ts" 3 6000000
TOTAL=$((TOTAL+1))
OLD_SIZE=$(file_size "$TEST_TMPDIR/swap_old.ts" 2>/dev/null || echo 0)
if [ "${OLD_SIZE:-0}" -lt 1000 ]; then
    echo -e "  ${GREEN}PASS${NC} hot-swap: old port 6210 no longer receives (${OLD_SIZE}b)"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} hot-swap: old port 6210 still receiving (${OLD_SIZE}b)"
    FAIL=$((FAIL+1))
fi

curl -s -X POST "$BASE/v1/flows/$SWAP_ID/stop" -o /dev/null
sleep 1
bg_kill "$SWAP_SRC"
curl -s -X DELETE "$BASE/v1/flows/$SWAP_ID" -o /dev/null

# ============================================================================
# D.8 Start With Zero Outputs, Then Hot-Add
# ============================================================================

echo ""
echo -e "${YEL}[D.8] Start With Zero Outputs, Then Hot-Add${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Zero Outputs",
  "source": {"name":"src","protocol":"udp","ingest_port":5220},
  "outputs": []
}')
ZERO_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "zero-out: flow created with 0 outputs" "201" "$(echo "$RESP" | tail -1)"

ZERO_SRC=$(start_udp_source 5220)
sleep 2

# Start flow with no outputs (relay runs, input feeds into relay, but nothing reads from it)
curl -s -X POST "$BASE/v1/flows/$ZERO_ID/start" -o /dev/null
sleep 3

FLOW_ST=$(curl -s "$BASE/v1/flows/$ZERO_ID" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])" 2>/dev/null)
check "zero-out: flow ACTIVE with 0 outputs" "ACTIVE" "$FLOW_ST"

# Hot-add an output to the running flow
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows/$ZERO_ID/outputs" -H "Content-Type: application/json" -d '{
  "name": "late_added",
  "protocol": "udp",
  "destination": "127.0.0.1",
  "port": 6220
}')
check "zero-out: hot-add output API" "200" "$(echo "$RESP" | tail -1)"

sleep 4

# Verify the hot-added output receives valid media
capture_udp 6220 "$TEST_TMPDIR/zero_out.ts" 4
check_media "zero-out: hot-added output receives media" "$TEST_TMPDIR/zero_out.ts"

curl -s -X POST "$BASE/v1/flows/$ZERO_ID/stop" -o /dev/null
sleep 1
bg_kill "$ZERO_SRC"
curl -s -X DELETE "$BASE/v1/flows/$ZERO_ID" -o /dev/null

# ============================================================================
# E. SRT Listener Transport (real media validation)
# ============================================================================

echo ""
echo -e "${YEL}[E] SRT Listener -> UDP Transport${NC}"

if [ "$HAS_SRT" = "true" ]; then
    RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
      "name": "SRT Test",
      "source": {"name":"srt_src","protocol":"srt-listener","ingest_port":5200},
      "outputs": [{"name":"srt_out","protocol":"udp","destination":"127.0.0.1","port":6200}]
    }')
    SRT_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
    SRT_STATUS=$(echo "$RESP" | tail -1)
    check "SRT flow created" "201" "$SRT_STATUS"

    # Start flow first (SRT listener must be ready before caller connects)
    curl -s -X POST "$BASE/v1/flows/$SRT_ID/start" -o /dev/null
    sleep 2

    # Send via SRT caller
    SRT_SRC=$(bg_start ffmpeg -hide_banner -loglevel error \
        -re -f lavfi -i "testsrc=size=320x240:rate=25" \
        -f lavfi -i "sine=frequency=440:sample_rate=44100" \
        -c:v libx264 -preset ultrafast -tune zerolatency -g 25 -b:v 500k \
        -c:a aac -b:a 128k \
        -f mpegts "srt://127.0.0.1:5200?mode=caller")
    sleep 4

    capture_udp 6200 "$TEST_TMPDIR/srt_relay.ts" 5
    check_media "SRT->UDP relay: valid H.264/AAC" "$TEST_TMPDIR/srt_relay.ts"

    curl -s -X POST "$BASE/v1/flows/$SRT_ID/stop" -o /dev/null
    sleep 1
    bg_kill "$SRT_SRC"
    curl -s -X DELETE "$BASE/v1/flows/$SRT_ID" -o /dev/null
else
    skip "SRT transport (ffmpeg lacks SRT support)"
    skip "SRT flow created"
    skip "SRT media validation"
fi

# ============================================================================
# F. SRT Encrypted End-to-End
# ============================================================================

echo ""
echo -e "${YEL}[F] SRT Encrypted Transport (end-to-end)${NC}"

if [ "$HAS_SRT" = "true" ]; then
    PASSPHRASE="mroute_test_pass_1234"

    RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d "{
      \"name\": \"SRT Encrypted\",
      \"source\": {
        \"name\":\"enc_src\",
        \"protocol\":\"srt-listener\",
        \"ingest_port\":5400,
        \"decryption\":{\"algorithm\":\"aes128\",\"passphrase\":\"${PASSPHRASE}\"}
      },
      \"outputs\": [{\"name\":\"enc_out\",\"protocol\":\"udp\",\"destination\":\"127.0.0.1\",\"port\":6400}]
    }")
    ENC_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
    ENC_STATUS=$(echo "$RESP" | tail -1)
    check "SRT encrypted flow created" "201" "$ENC_STATUS"

    curl -s -X POST "$BASE/v1/flows/$ENC_ID/start" -o /dev/null
    sleep 2

    # Send via SRT caller with matching passphrase
    ENC_SRC=$(bg_start ffmpeg -hide_banner -loglevel error \
        -re -f lavfi -i "testsrc=size=320x240:rate=25" \
        -f lavfi -i "sine=frequency=660:sample_rate=44100" \
        -c:v libx264 -preset ultrafast -tune zerolatency -g 25 -b:v 500k \
        -c:a aac -b:a 128k \
        -f mpegts "srt://127.0.0.1:5400?mode=caller&passphrase=${PASSPHRASE}&pbkeylen=16")
    sleep 4

    capture_udp 6400 "$TEST_TMPDIR/srt_enc_relay.ts" 5
    check_media "SRT encrypted relay: valid H.264/AAC" "$TEST_TMPDIR/srt_enc_relay.ts"

    curl -s -X POST "$BASE/v1/flows/$ENC_ID/stop" -o /dev/null
    sleep 1
    bg_kill "$ENC_SRC"
    curl -s -X DELETE "$BASE/v1/flows/$ENC_ID" -o /dev/null
else
    skip "SRT encrypted transport (ffmpeg lacks SRT support)"
    skip "SRT encrypted flow created"
    skip "SRT encrypted media validation"
fi

# ============================================================================
# G. SRT Encryption API Validation
# ============================================================================

echo ""
echo -e "${YEL}[G] SRT Encryption Validation (API)${NC}"

# Short passphrase (<10 chars) should fail
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Bad Passphrase",
  "source": {"name":"src","protocol":"srt-listener","ingest_port":5401,"decryption":{"passphrase":"short"}},
  "outputs": []
}')
check "short passphrase rejected" "400" "$STATUS"

# Invalid algorithm
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Bad Algorithm",
  "source": {"name":"src","protocol":"srt-listener","ingest_port":5402,"decryption":{"algorithm":"aes512","passphrase":"longenoughpassphrase"}},
  "outputs": []
}')
check "invalid algorithm rejected" "400" "$STATUS"

# ============================================================================
# H. Source Failover (real media validation)
# ============================================================================

echo ""
echo -e "${YEL}[H] Source Failover${NC}"
bg_killall

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
FO_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Add backup source
curl -s -X POST "$BASE/v1/flows/$FO_ID/source" -H "Content-Type: application/json" \
  -d '{"name":"backup","protocol":"udp","ingest_port":5301}' -o /dev/null

# Start primary sender
FO_PRIMARY=$(start_udp_source 5300 500k 440)
sleep 1

# Start flow
curl -s -X POST "$BASE/v1/flows/$FO_ID/start" -o /dev/null
sleep 3

# Verify primary is active
ACTIVE=$(curl -s "$BASE/v1/flows/$FO_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['active_source'])" 2>/dev/null)
check "failover: primary active" "primary" "$ACTIVE"

# Capture output while primary is running to confirm data flows
capture_udp 6300 "$TEST_TMPDIR/fo_before.ts" 3
check_media "failover: media before switch" "$TEST_TMPDIR/fo_before.ts" 3000

# Start backup sender BEFORE killing primary
FO_BACKUP=$(start_udp_source 5301 500k 880)
sleep 2

# Kill primary to trigger failover
bg_kill "$FO_PRIMARY"
echo "  ... waiting for failover (UDP timeout + switch)..."
sleep 15

# Verify backup is now active
ACTIVE2=$(curl -s "$BASE/v1/flows/$FO_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['active_source'])" 2>/dev/null)
check "failover: backup active after kill" "backup" "$ACTIVE2"

# Check failover count > 0
FO_COUNT=$(curl -s "$BASE/v1/flows/$FO_ID/metrics" | python3 -c "import sys,json; print(json.load(sys.stdin)['failover_count'])" 2>/dev/null)
TOTAL=$((TOTAL+1))
if [ "$FO_COUNT" -gt "0" ]; then
    echo -e "  ${GREEN}PASS${NC} failover_count = $FO_COUNT"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} failover_count = $FO_COUNT (expected > 0)"
    FAIL=$((FAIL+1))
fi

# Wait for new input FFmpeg to stabilize on backup source before capturing
sleep 5

# Capture output after failover to verify data is still flowing with proper media
capture_udp 6300 "$TEST_TMPDIR/fo_after.ts" 6
check_media "failover: media flowing after switch" "$TEST_TMPDIR/fo_after.ts" 20000

curl -s -X POST "$BASE/v1/flows/$FO_ID/stop" -o /dev/null
sleep 1
bg_kill "$FO_BACKUP"
curl -s -X DELETE "$BASE/v1/flows/$FO_ID" -o /dev/null

# ============================================================================
# I. MERGE Mode Validation
# ============================================================================

echo ""
echo -e "${YEL}[I] MERGE Mode Validation${NC}"

# MERGE mode with SRT (non-merge-capable) should fail
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Bad MERGE",
  "source": {"name":"primary","protocol":"srt-listener","ingest_port":5500},
  "sources": [{"name":"backup","protocol":"srt-listener","ingest_port":5501}],
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6500}],
  "source_failover_config": {"state":"ENABLED","failover_mode":"MERGE"}
}')
check "MERGE rejects SRT protocol" "400" "$STATUS"

# MERGE mode with RTP should succeed
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "MERGE RTP",
  "source": {"name":"rtp_a","protocol":"rtp","ingest_port":5510},
  "sources": [{"name":"rtp_b","protocol":"rtp","ingest_port":5511}],
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6510}],
  "source_failover_config": {"state":"ENABLED","failover_mode":"MERGE","recovery_window":200}
}')
MERGE_STATUS=$(echo "$RESP" | tail -1)
MERGE_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "MERGE mode with RTP created" "201" "$MERGE_STATUS"
curl -s -X DELETE "$BASE/v1/flows/$MERGE_ID" -o /dev/null

# MERGE with single source degrades gracefully
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "MERGE Single",
  "source": {"name":"only_one","protocol":"rtp","ingest_port":5520},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6520}],
  "source_failover_config": {"state":"ENABLED","failover_mode":"MERGE"}
}')
MERGE_SINGLE_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "MERGE single source creates OK" "201" "$(echo "$RESP" | tail -1)"
curl -s -X POST "$BASE/v1/flows/$MERGE_SINGLE_ID/start" -o /dev/null
sleep 1
MERGE_SINGLE_ST=$(curl -s "$BASE/v1/flows/$MERGE_SINGLE_ID" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])" 2>/dev/null)
check "MERGE single source degrades to ACTIVE" "ACTIVE" "$MERGE_SINGLE_ST"
curl -s -X POST "$BASE/v1/flows/$MERGE_SINGLE_ID/stop" -o /dev/null
sleep 1
curl -s -X DELETE "$BASE/v1/flows/$MERGE_SINGLE_ID" -o /dev/null

# ============================================================================
# J. Monitoring: Offline Flow
# ============================================================================

echo ""
echo -e "${YEL}[J.1] Monitoring (offline flow)${NC}"

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Monitor Offline",
  "source": {"name":"src","protocol":"udp","ingest_port":5700},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6700}],
  "source_monitor_config": {"thumbnail_enabled": true, "content_quality_enabled": true}
}')
MON_OFF_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$MON_OFF_ID/thumbnail")
check "thumbnail 404 when offline" "404" "$STATUS"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$MON_OFF_ID/metadata")
check "metadata 404 when offline" "404" "$STATUS"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$MON_OFF_ID/content-quality")
check "content-quality 404 when offline" "404" "$STATUS"

curl -s -X DELETE "$BASE/v1/flows/$MON_OFF_ID" -o /dev/null

# ============================================================================
# K. Monitoring: Live Flow (real content validation)
# ============================================================================

echo ""
echo -e "${YEL}[J.2] Monitoring (live flow - real validation)${NC}"
bg_killall

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Live Monitor",
  "source": {"name":"src","protocol":"udp","ingest_port":5800},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6800}],
  "source_monitor_config": {
    "thumbnail_enabled": true,
    "content_quality_enabled": true,
    "thumbnail_interval_sec": 2
  }
}')
LIVE_MON_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Start source
MON_SRC=$(start_udp_source 5800)
sleep 2

# Start flow
curl -s -X POST "$BASE/v1/flows/$LIVE_MON_ID/start" -o /dev/null

# Wait for all monitors to produce data:
# - metadata: 3s initial + ~5s probe = ~8s
# - thumbnail: 5s initial + capture = ~7s
# - content quality: 5s initial + 5s analysis = ~11s
echo "  ... waiting 15s for monitoring data..."
sleep 15

# --- Metadata validation ---
TOTAL=$((TOTAL+1))
META_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$LIVE_MON_ID/metadata")
if [ "$META_STATUS" = "200" ]; then
    META_JSON=$(curl -s "$BASE/v1/flows/$LIVE_MON_ID/metadata")
    META_INFO=$(echo "$META_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
streams = d.get('streams', [])
vs = [s for s in streams if s.get('stream_type') == 'video']
a_s = [s for s in streams if s.get('stream_type') == 'audio']
vc = vs[0].get('codec','?') if vs else 'none'
vr = f\"{vs[0].get('width','?')}x{vs[0].get('height','?')}\" if vs else 'none'
ac = a_s[0].get('codec','?') if a_s else 'none'
print(f'{len(vs)} {len(a_s)} {vc} {ac} {vr}')
" 2>/dev/null)
    MVCOUNT=$(echo "$META_INFO" | cut -d' ' -f1)
    MACOUNT=$(echo "$META_INFO" | cut -d' ' -f2)
    MVCODEC=$(echo "$META_INFO" | cut -d' ' -f3)
    MVRES=$(echo "$META_INFO" | cut -d' ' -f5)
    if [ "$MVCOUNT" -ge 1 ] && [ "$MACOUNT" -ge 1 ]; then
        echo -e "  ${GREEN}PASS${NC} Metadata: ${MVCODEC}@${MVRES} + audio (${MVCOUNT}v/${MACOUNT}a streams)"
        PASS=$((PASS+1))
    else
        echo -e "  ${RED}FAIL${NC} Metadata: expected video+audio, got ${MVCOUNT}v/${MACOUNT}a"
        FAIL=$((FAIL+1))
    fi
else
    echo -e "  ${RED}FAIL${NC} Metadata endpoint returned $META_STATUS (expected 200)"
    FAIL=$((FAIL+1))
fi

# Validate metadata video codec is h264
TOTAL=$((TOTAL+1))
if [ "${MVCODEC:-none}" = "h264" ]; then
    echo -e "  ${GREEN}PASS${NC} Metadata video codec = h264"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} Metadata video codec = '${MVCODEC:-none}' (expected h264)"
    FAIL=$((FAIL+1))
fi

# --- Thumbnail validation ---
TOTAL=$((TOTAL+1))
THUMB_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$LIVE_MON_ID/thumbnail")
if [ "$THUMB_STATUS" = "200" ]; then
    THUMB_JSON=$(curl -s "$BASE/v1/flows/$LIVE_MON_ID/thumbnail")
    # Check that base64 data is present and starts with JPEG magic (/9j = 0xFF 0xD8)
    THUMB_CHECK=$(echo "$THUMB_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
data = d.get('data', '')
ts = d.get('timestamp', '')
if len(data) > 100 and data[:3] == '/9j':
    print(f'OK:{len(data)}b,ts={ts}')
elif len(data) > 100:
    print(f'NOMAGIC:{data[:4]}')
else:
    print(f'SMALL:{len(data)}b')
" 2>/dev/null)
    if [[ "$THUMB_CHECK" == OK:* ]]; then
        echo -e "  ${GREEN}PASS${NC} Thumbnail: valid JPEG [$THUMB_CHECK]"
        PASS=$((PASS+1))
    else
        echo -e "  ${RED}FAIL${NC} Thumbnail: invalid data [$THUMB_CHECK]"
        FAIL=$((FAIL+1))
    fi
else
    echo -e "  ${RED}FAIL${NC} Thumbnail endpoint returned $THUMB_STATUS (expected 200)"
    FAIL=$((FAIL+1))
fi

# --- Content Quality validation ---
TOTAL=$((TOTAL+1))
CQ_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/flows/$LIVE_MON_ID/content-quality")
if [ "$CQ_STATUS" = "200" ]; then
    CQ_JSON=$(curl -s "$BASE/v1/flows/$LIVE_MON_ID/content-quality")
    CQ_CHECK=$(echo "$CQ_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
vp = d.get('video_stream_present', False)
ap = d.get('audio_stream_present', False)
bf = d.get('black_frame_detected', True)
ff = d.get('frozen_frame_detected', True)
sa = d.get('silent_audio_detected', True)
# testsrc is colorful (not black), animated (not frozen), sine is audible (not silent)
issues = []
if not vp: issues.append('no_video')
if not ap: issues.append('no_audio')
if bf: issues.append('false_black')
if ff: issues.append('false_frozen')
if sa: issues.append('false_silent')
if not issues:
    print(f'OK:video={vp},audio={ap},black={bf},frozen={ff},silent={sa}')
else:
    print(f'ISSUES:{\",\".join(issues)}')
" 2>/dev/null)
    if [[ "$CQ_CHECK" == OK:* ]]; then
        echo -e "  ${GREEN}PASS${NC} Content quality: correct detection [$CQ_CHECK]"
        PASS=$((PASS+1))
    else
        echo -e "  ${RED}FAIL${NC} Content quality: wrong detection [$CQ_CHECK]"
        FAIL=$((FAIL+1))
    fi
else
    echo -e "  ${RED}FAIL${NC} Content quality endpoint returned $CQ_STATUS (expected 200)"
    FAIL=$((FAIL+1))
fi

curl -s -X POST "$BASE/v1/flows/$LIVE_MON_ID/stop" -o /dev/null
sleep 1
bg_kill "$MON_SRC"
curl -s -X DELETE "$BASE/v1/flows/$LIVE_MON_ID" -o /dev/null

# ============================================================================
# L. Config & Edge Cases
# ============================================================================

echo ""
echo -e "${YEL}[K] Cache-Control Header${NC}"
TOTAL=$((TOTAL+1))
CACHE=$(curl -s -D - -o /dev/null "$BASE/health" | grep -i "cache-control" | tr -d '\r')
if echo "$CACHE" | grep -qi "no-store"; then
    echo -e "  ${GREEN}PASS${NC} Cache-Control: no-store present"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} Missing Cache-Control header ($CACHE)"
    FAIL=$((FAIL+1))
fi

echo ""
echo -e "${YEL}[L] Event Cleanup on Delete${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Event Cleanup",
  "source": {"name":"src","protocol":"udp","ingest_port":5050},
  "outputs": [{"name":"o","protocol":"udp","destination":"127.0.0.1","port":6050}]
}')
CLEANUP_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
curl -s -X POST "$BASE/v1/flows/$CLEANUP_ID/start" -o /dev/null
sleep 1
curl -s -X POST "$BASE/v1/flows/$CLEANUP_ID/stop" -o /dev/null
sleep 1
EVT_BEFORE=$(curl -s "$BASE/v1/flows/$CLEANUP_ID/events" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['events']))")
curl -s -X DELETE "$BASE/v1/flows/$CLEANUP_ID" -o /dev/null
TOTAL=$((TOTAL+1))
if [ "$EVT_BEFORE" -gt "0" ]; then
    echo -e "  ${GREEN}PASS${NC} Events existed before delete ($EVT_BEFORE)"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} No events before delete"
    FAIL=$((FAIL+1))
fi

echo ""
echo -e "${YEL}[M] Double Stop (idempotency)${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Double Stop",
  "source": {"name":"src","protocol":"udp","ingest_port":5060},
  "outputs": [{"name":"o","protocol":"udp","destination":"127.0.0.1","port":6060}]
}')
DSTOP_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
curl -s -X POST "$BASE/v1/flows/$DSTOP_ID/start" -o /dev/null
sleep 1
curl -s -X POST "$BASE/v1/flows/$DSTOP_ID/stop" -o /dev/null
sleep 1
STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/flows/$DSTOP_ID/stop")
check "double stop is safe (200)" "200" "$STATUS"
curl -s -X DELETE "$BASE/v1/flows/$DSTOP_ID" -o /dev/null

echo ""
echo -e "${YEL}[N] Maintenance Window Config${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Maintenance",
  "source": {"name":"src","protocol":"udp","ingest_port":5900},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6900}],
  "maintenance_window": {"day_of_week":"Sunday","start_hour":3}
}')
MW_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
MW_STATUS=$(echo "$RESP" | tail -1)
check "maintenance window flow created" "201" "$MW_STATUS"
MW_DAY=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin).get('maintenance_window',{}).get('day_of_week',''))")
check "maintenance day=Sunday" "Sunday" "$MW_DAY"
MW_HOUR=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin).get('maintenance_window',{}).get('start_hour',0))")
check "maintenance hour=3" "3" "$MW_HOUR"
curl -s -X DELETE "$BASE/v1/flows/$MW_ID" -o /dev/null

echo ""
echo -e "${YEL}[O] Monitor Config Persistence${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Monitor Config",
  "source": {"name":"src","protocol":"udp","ingest_port":5600},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6600}],
  "source_monitor_config": {"thumbnail_enabled":true,"content_quality_enabled":true,"thumbnail_interval_sec":5}
}')
MON_CFG_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "monitor config flow created" "201" "$(echo "$RESP" | tail -1)"
HAS_MON=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; d=json.load(sys.stdin); print('true' if d.get('source_monitor_config',{}).get('thumbnail_enabled') else 'false')")
check "monitor config thumbnail_enabled saved" "true" "$HAS_MON"
curl -s -X DELETE "$BASE/v1/flows/$MON_CFG_ID" -o /dev/null

# ============================================================================
# P. Enhanced Metrics (with real flow)
# ============================================================================

echo ""
echo -e "${YEL}[P] Enhanced Metrics${NC}"
bg_killall

ENH_SRC=$(start_udp_source 5950)
sleep 1

RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Enhanced Metrics",
  "source": {"name":"src","protocol":"udp","ingest_port":5950},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6950}]
}')
ENH_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
curl -s -X POST "$BASE/v1/flows/$ENH_ID/start" -o /dev/null
sleep 4

METRICS=$(curl -s "$BASE/v1/flows/$ENH_ID/metrics")

# Check uptime
TOTAL=$((TOTAL+1))
HAS_UPTIME=$(echo "$METRICS" | python3 -c "import sys,json; m=json.load(sys.stdin); print('true' if m.get('uptime_seconds',0) > 0 else 'false')" 2>/dev/null)
if [ "$HAS_UPTIME" = "true" ]; then
    UPTIME=$(echo "$METRICS" | python3 -c "import sys,json; print(json.load(sys.stdin).get('uptime_seconds',0))" 2>/dev/null)
    echo -e "  ${GREEN}PASS${NC} uptime_seconds present (${UPTIME}s)"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} uptime_seconds missing or 0"
    FAIL=$((FAIL+1))
fi

# Check source metrics fields
HAS_SRC_FIELDS=$(echo "$METRICS" | python3 -c "
import sys,json
m=json.load(sys.stdin)
sm = m.get('source_metrics',[{}])[0] if m.get('source_metrics') else {}
has_fields = 'packets_received' in sm and 'packets_lost' in sm and 'jitter_ms' in sm
print('true' if has_fields else 'false')
" 2>/dev/null)
check "source metrics have packets/jitter fields" "true" "$HAS_SRC_FIELDS"

# Check output metrics fields
HAS_OUT_FIELDS=$(echo "$METRICS" | python3 -c "
import sys,json
m=json.load(sys.stdin)
om = m.get('output_metrics',[{}])[0] if m.get('output_metrics') else {}
has_fields = 'disconnections' in om and 'packets_sent' in om
print('true' if has_fields else 'false')
" 2>/dev/null)
check "output metrics have disconnections/packets fields" "true" "$HAS_OUT_FIELDS"

curl -s -X POST "$BASE/v1/flows/$ENH_ID/stop" -o /dev/null
sleep 1
bg_kill "$ENH_SRC"
curl -s -X DELETE "$BASE/v1/flows/$ENH_ID" -o /dev/null

# ============================================================================
# Q. SRT Output Encryption Config
# ============================================================================

echo ""
echo -e "${YEL}[Q] SRT Output Encryption Config${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "SRT Out Encrypted",
  "source": {"name":"src","protocol":"udp","ingest_port":5960},
  "outputs": [{
    "name":"srt_enc_out",
    "protocol":"srt-listener",
    "port":6960,
    "encryption":{"algorithm":"aes256","passphrase":"my_output_passphrase_for_srt"}
  }]
}')
ENC_OUT_STATUS=$(echo "$RESP" | tail -1)
check "SRT encrypted output created" "201" "$ENC_OUT_STATUS"
HAS_ENC=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; d=json.load(sys.stdin); print('true' if d['outputs'][0].get('encryption',{}).get('algorithm')=='aes256' else 'false')")
check "output encryption config saved" "true" "$HAS_ENC"
ENC_OUT_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
curl -s -X DELETE "$BASE/v1/flows/$ENC_OUT_ID" -o /dev/null

# ============================================================================
# R. Global Events
# ============================================================================

echo ""
echo -e "${YEL}[R] Global Events${NC}"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/v1/flows" -H "Content-Type: application/json" -d '{
  "name": "Events Test",
  "source": {"name":"src","protocol":"udp","ingest_port":5970},
  "outputs": [{"name":"out","protocol":"udp","destination":"127.0.0.1","port":6970}]
}')
EVT_TEST_ID=$(echo "$RESP" | sed '$d' | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
curl -s -X POST "$BASE/v1/flows/$EVT_TEST_ID/start" -o /dev/null
sleep 1
curl -s -X POST "$BASE/v1/flows/$EVT_TEST_ID/stop" -o /dev/null
sleep 1
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/v1/events")
check "GET /v1/events" "200" "$STATUS"
EVT_GLOBAL=$(curl -s "$BASE/v1/events" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['events']))")
TOTAL=$((TOTAL+1))
if [ "$EVT_GLOBAL" -gt "0" ]; then
    echo -e "  ${GREEN}PASS${NC} Global events have entries ($EVT_GLOBAL)"
    PASS=$((PASS+1))
else
    echo -e "  ${RED}FAIL${NC} Global events empty"
    FAIL=$((FAIL+1))
fi
curl -s -X DELETE "$BASE/v1/flows/$EVT_TEST_ID" -o /dev/null

# ============================================================================
# Final cleanup of any remaining test flows
# ============================================================================

bg_killall
for FID in $(curl -s "$BASE/v1/flows" 2>/dev/null | python3 -c "import sys,json; [print(f['id']) for f in json.load(sys.stdin).get('flows',[])]" 2>/dev/null); do
    curl -s -X POST "$BASE/v1/flows/$FID/stop" -o /dev/null 2>/dev/null
    curl -s -X DELETE "$BASE/v1/flows/$FID" -o /dev/null 2>/dev/null
done

# ============================================================================
# Results
# ============================================================================

echo ""
echo -e "${YEL}============================================${NC}"
echo -e "  ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}, ${YEL}$SKIP skipped${NC}, $TOTAL total"
echo -e "${YEL}============================================${NC}"

if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "Server log (last 30 lines):"
    tail -30 "$TEST_TMPDIR/server.log" 2>/dev/null
fi

exit $FAIL
