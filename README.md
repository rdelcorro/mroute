# mroute

A live video transport engine written in Go. mroute ingests live media streams from one or more sources, applies protocol translation, source redundancy, and content-aware merging, then fans out to multiple destinations -- all without transcoding. Streams pass through as-is (`-c copy`), preserving original quality at minimal CPU cost.

## Features

- **Multi-protocol transport** -- SRT (listener and caller modes), RTP, RTP-FEC, RIST, UDP, and RTMP
- **Source failover** -- automatic switchover to a backup source when the primary fails, with configurable recovery back to primary
- **SMPTE ST 2022-7 hitless merge** -- content-aware RTP packet merging from two redundant sources using sequence-number deduplication and a configurable recovery window
- **Fan-out** -- up to 50 simultaneous outputs per flow, each with independent protocol and destination
- **SRT encryption** -- AES-128, AES-192, or AES-256 passphrase encryption on both ingest (decryption) and output (encryption)
- **Source monitoring** -- thumbnail capture, ffprobe-based stream metadata extraction, and content quality detection (black frames, frozen frames, silent audio)
- **Maintenance windows** -- scheduled automatic restarts for long-running flows
- **Real-time metrics** -- per-source and per-output health, bitrate, packet counts, failover count, merge statistics, and uptime
- **Event system** -- persistent event log per flow with severity levels, queryable via API
- **Telegram notifications** -- optional real-time alerts for flow events, failovers, startup, and shutdown
- **SQLite persistence** -- flows and events survive restarts; WAL mode for concurrent reads
- **REST API** -- full lifecycle management: create, start, stop, delete, add/remove sources and outputs, query metrics and events

## Architecture

```
                                 +-----------+
                                 |  REST API |
                                 |  :8080    |
                                 +-----+-----+
                                       |
                              +--------+--------+
                              |  Flow Manager   |
                              |  (persistence)  |
                              +--------+--------+
                                       |
               +-----------+-----------+-----------+
               |           |                       |
        +------+------+  +-+----------+  +--------+--------+
        |  Transport  |  |  Monitor   |  |  Telegram Bot   |
        |  Engine     |  |  (sidecar) |  |  (notifications)|
        +------+------+  +-----+------+  +-----------------+
               |                |
       +-------+-------+  +----+----+
       |               |  | ffprobe |
   +---+---+     +-----+--+--------+
   | FFmpeg |     | RTP Merger      |
   | (-c copy)   | (ST 2022-7)     |
   +-------+     +-----------------+
```

### Packages

| Package | Path | Purpose |
|---------|------|---------|
| `main` | `cmd/mroute/` | Entry point, signal handling, component wiring |
| `api` | `internal/api/` | HTTP server, REST endpoints, request routing |
| `flow` | `internal/flow/` | Flow lifecycle, SQLite persistence, maintenance scheduler |
| `transport` | `internal/transport/` | FFmpeg process management, failover logic, metrics collection |
| `merge` | `internal/merge/` | SMPTE ST 2022-7 RTP packet merger |
| `monitor` | `internal/monitor/` | Thumbnail capture, ffprobe metadata, content quality detection |
| `config` | `internal/config/` | YAML config loading with environment variable overrides |
| `telegram` | `internal/telegram/` | Telegram Bot API integration for notifications |
| `types` | `pkg/types/` | Shared data types, validation, URI builders |

## Requirements

- **Go 1.22+**
- **FFmpeg** (with SRT, RIST support compiled in) -- used as the transport backend
- **ffprobe** -- used for stream metadata probing and primary source recovery detection
- **SQLite3** -- linked via `github.com/mattn/go-sqlite3` (requires CGo)

## Building

```bash
go build -o mroute ./cmd/mroute
```

Or with static linking:

```bash
CGO_ENABLED=1 go build -ldflags="-s -w" -o mroute ./cmd/mroute
```

## Configuration

mroute reads `config.yaml` from the working directory by default. Pass `-config /path/to/config.yaml` to override.

```yaml
api:
  host: 0.0.0.0
  port: 8080

storage:
  dsn: "sqlite:./mroute.db"

ffmpeg:
  path: ffmpeg
  probe_path: ffprobe
  max_flows: 20
  health_timeout_ms: 500    # failover health threshold

telegram:
  enabled: true
  bot_token: "123456:ABC-DEF..."
  chat_id: "-1001234567890"
```

### Environment Variables

| Variable | Effect |
|----------|--------|
| `TELEGRAM_BOT_TOKEN` | Enables Telegram notifications |
| `TELEGRAM_CHAT_ID` | Target chat/group for notifications |
| `MROUTE_PORT` | Overrides `api.port` |

If no config file is found, sensible defaults are used (port 8080, local SQLite, system FFmpeg).

## Running

```bash
./mroute
# or with custom config
./mroute -config /etc/mroute/config.yaml
```

mroute handles `SIGINT` and `SIGTERM` for graceful shutdown: all running flows are stopped, FFmpeg processes are terminated with SIGTERM (with a 5-second kill timeout), the Telegram message queue is drained, and the database is closed.

## API Reference

Base URL: `http://localhost:8080`

### Health

```
GET /health
```

Returns server status and flow counts.

```json
{
  "status": "ok",
  "total_flows": 5,
  "active_flows": 2
}
```

### Flows

#### Create Flow

```
POST /v1/flows
```

```json
{
  "name": "My Live Feed",
  "description": "Primary ingest from studio",
  "source": {
    "name": "primary",
    "protocol": "srt-listener",
    "ingest_port": 5000,
    "max_latency": 200,
    "decryption": {
      "algorithm": "aes128",
      "passphrase": "my_secret_passphrase"
    }
  },
  "outputs": [
    {
      "name": "cdn_feed",
      "protocol": "srt-caller",
      "destination": "cdn.example.com",
      "port": 9000,
      "max_latency": 300,
      "encryption": {
        "algorithm": "aes256",
        "passphrase": "output_secret_passphrase"
      }
    },
    {
      "name": "monitoring",
      "protocol": "udp",
      "destination": "10.0.0.50",
      "port": 6000
    }
  ],
  "source_failover_config": {
    "state": "ENABLED",
    "failover_mode": "FAILOVER",
    "source_priority": {
      "primary_source": "primary"
    }
  },
  "source_monitor_config": {
    "thumbnail_enabled": true,
    "content_quality_enabled": true,
    "thumbnail_interval_sec": 5
  },
  "maintenance_window": {
    "day_of_week": "Sunday",
    "start_hour": 3
  }
}
```

Returns `201 Created` with the full flow object including generated `id`.

#### List Flows

```
GET /v1/flows
```

Returns `{"flows": [...]}`.

#### Get Flow

```
GET /v1/flows/{id}
```

#### Delete Flow

```
DELETE /v1/flows/{id}
```

Stops the flow if running and removes all associated events. Returns `204 No Content`.

#### Start Flow

```
POST /v1/flows/{id}/start
```

Launches FFmpeg transport. The flow transitions through `STARTING` to `ACTIVE`.

#### Stop Flow

```
POST /v1/flows/{id}/stop
```

Terminates FFmpeg transport. The flow returns to `STANDBY`. Idempotent -- stopping an already-stopped flow is safe.

### Sources

Flows support up to 2 sources (one primary, one backup) for redundancy.

#### Add Source

```
POST /v1/flows/{id}/source
```

```json
{
  "name": "backup",
  "protocol": "srt-listener",
  "ingest_port": 5001
}
```

#### Remove Source

```
DELETE /v1/flows/{id}/source/{name}
```

The last remaining source cannot be removed.

### Outputs

#### Add Output

```
POST /v1/flows/{id}/outputs
```

```json
{
  "name": "new_output",
  "protocol": "rtp",
  "destination": "10.0.0.100",
  "port": 7000
}
```

#### Update Output

```
PUT /v1/flows/{id}/outputs/{name}
```

Partial update -- only specified fields are changed.

#### Remove Output

```
DELETE /v1/flows/{id}/outputs/{name}
```

### Metrics

```
GET /v1/flows/{id}/metrics
```

Returns real-time metrics for a running flow:

```json
{
  "flow_id": "flow_abc123",
  "active_source": "primary",
  "status": "ACTIVE",
  "uptime_seconds": 3600,
  "failover_count": 0,
  "merge_active": false,
  "source_metrics": [
    {
      "name": "primary",
      "status": "CONNECTED",
      "connected": true,
      "bitrate_kbps": 5000,
      "packets_received": 180000,
      "packets_lost": 3,
      "packets_recovered": 0,
      "round_trip_time_ms": 12,
      "jitter_ms": 2,
      "source_selected": true
    }
  ],
  "output_metrics": [
    {
      "name": "cdn_feed",
      "status": "CONNECTED",
      "connected": true,
      "bitrate_kbps": 5000,
      "packets_sent": 180000,
      "disconnections": 0
    }
  ],
  "timestamp": "2025-01-15T10:30:00Z"
}
```

When MERGE mode is active, `merge_stats` is included:

```json
{
  "merge_stats": {
    "source_a_packets": 90000,
    "source_a_gap_fills": 85000,
    "source_b_packets": 90000,
    "source_b_gap_fills": 5000,
    "output_packets": 90000,
    "missing_packets": 0
  }
}
```

### Source Monitoring

#### Thumbnail

```
GET /v1/flows/{id}/thumbnail
```

Returns a base64-encoded JPEG frame captured from the source:

```json
{
  "flow_id": "flow_abc123",
  "data": "/9j/4AAQSkZJRg...",
  "timestamp": "2025-01-15T10:30:00Z"
}
```

Requires `source_monitor_config.thumbnail_enabled: true`.

#### Stream Metadata

```
GET /v1/flows/{id}/metadata
```

Returns ffprobe-extracted stream information:

```json
{
  "flow_id": "flow_abc123",
  "programs": [
    {"program_id": 1, "program_name": "Service01"}
  ],
  "streams": [
    {
      "index": 0,
      "stream_type": "video",
      "codec": "h264",
      "profile": "High",
      "width": 1920,
      "height": 1080,
      "frame_rate": "25/1",
      "bit_rate": 5000000
    },
    {
      "index": 1,
      "stream_type": "audio",
      "codec": "aac",
      "channels": 2,
      "sample_rate": 48000,
      "bit_rate": 128000
    }
  ]
}
```

Always available for running flows (ffprobe runs automatically).

#### Content Quality

```
GET /v1/flows/{id}/content-quality
```

```json
{
  "black_frame_detected": false,
  "frozen_frame_detected": false,
  "silent_audio_detected": false,
  "video_stream_present": true,
  "audio_stream_present": true,
  "last_check_time": "2025-01-15T10:30:00Z"
}
```

Requires `source_monitor_config.content_quality_enabled: true`. Uses FFmpeg's `blackdetect`, `freezedetect`, and `silencedetect` filters on a rolling 5-second analysis window.

### Events

#### Flow Events

```
GET /v1/flows/{id}/events?limit=50
```

#### Global Events

```
GET /v1/events?limit=100
```

Returns events across all flows, ordered by timestamp descending. Maximum limit: 10,000. Events are pruned to 1,000 per flow automatically.

Event types include: `flow_started`, `flow_stopped`, `source_active`, `source_failover`, `source_error`, `source_retry`, `source_eof`, `primary_recovered`, `merge_started`, `merge_error`, `merge_eof`, `connection_error`, `maintenance_restart`.

## Protocols

### SRT (Secure Reliable Transport)

SRT provides reliable, low-latency transport over unpredictable networks with built-in encryption.

- **`srt-listener`** -- mroute opens a port and waits for incoming SRT connections. Use for ingest from remote encoders.
- **`srt-caller`** -- mroute initiates an SRT connection to a remote address. Use when connecting to an SRT listener at a known address.

SRT parameters: `max_latency` (ms), `max_bitrate` (bytes/sec for `maxbw`), `stream_id`, and encryption settings.

### RTP

Raw RTP over UDP. Used for point-to-point links and as the basis for MERGE mode.

### RTP-FEC

RTP with Forward Error Correction headers. Treated identically to RTP on the transport layer; compatible with MERGE mode.

### RIST

Reliable Internet Stream Transport. Adds ARQ-based error recovery to MPEG-TS over UDP. Compatible with MERGE mode.

### UDP

Plain MPEG-TS over UDP. The simplest transport -- no reliability, no encryption. Use for local links.

### RTMP

Real-Time Messaging Protocol. Used for ingest from software encoders (OBS, Wirecast) and output to CDN ingest points. Supports stream keys via the `stream_id` (source) or `stream_key` (output) fields.

## Source Redundancy

### Failover Mode

When `source_failover_config.failover_mode` is `FAILOVER`, mroute runs on the primary source and automatically switches to the backup if the primary fails (FFmpeg process exits or loses data).

**Auto-recovery**: If a `source_priority.primary_source` is configured, mroute continuously probes the primary source using ffprobe. When the primary recovers, it automatically switches back by sending SIGTERM to the current FFmpeg process.

### Merge Mode (SMPTE ST 2022-7)

When `failover_mode` is `MERGE`, mroute runs both sources simultaneously and performs hitless merging:

1. Both source streams are received on separate UDP ports
2. RTP packets are deduplicated by sequence number
3. Gaps in one stream are filled by packets from the other
4. The merged output is written to a local UDP port
5. FFmpeg reads from the merged stream and fans out to all outputs

The `recovery_window` (milliseconds, default 100) controls how long to wait for a missing packet before declaring it lost. MERGE mode requires RTP-based protocols (`rtp`, `rtp-fec`, or `rist`).

Merge statistics are available in the metrics endpoint: per-source packet counts, unique contributions, duplicates, total output packets, and packets lost by both sources.

## SRT Encryption

Both sources and outputs support SRT passphrase encryption:

```json
{
  "algorithm": "aes256",
  "passphrase": "my_secret_passphrase"
}
```

- **Algorithms**: `aes128`, `aes192`, `aes256`
- **Passphrase**: 10-79 characters
- On sources, this is configured as `decryption` (decrypting incoming encrypted streams)
- On outputs, this is configured as `encryption` (encrypting outgoing streams)

## Maintenance Windows

Configure automatic restarts for long-running flows:

```json
{
  "maintenance_window": {
    "day_of_week": "Sunday",
    "start_hour": 3
  }
}
```

The maintenance scheduler checks every minute. When a flow has been running for 24+ hours and the current UTC time matches the configured day and hour, the flow is stopped and restarted. This helps with FFmpeg memory stability on multi-day runs.

## Telegram Notifications

Set `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` environment variables (or configure in `config.yaml`) to receive real-time notifications for:

- Server startup and shutdown
- Flow started / stopped events
- Source failovers
- Connection errors
- Maintenance restarts

Messages are rate-limited to 1 per second and queued (up to 100 pending). The queue is drained on graceful shutdown.

## Flow Lifecycle

```
STANDBY --> STARTING --> ACTIVE --> STOPPING --> STANDBY
                           |
                           +--> ERROR (engine session ended unexpectedly)
```

- `STANDBY` -- flow is configured but not running
- `STARTING` -- FFmpeg process is being launched
- `ACTIVE` -- transport is running; metrics are available
- `ERROR` -- the engine session ended but the flow was not explicitly stopped
- Deleting a flow automatically stops it first

## Testing

An integration test script is included:

```bash
# Start mroute
./mroute &

# Run tests
bash test_mroute.sh
```

The test suite covers:

- Health endpoint
- Flow CRUD (create, get, list, delete)
- Output management (add, remove)
- Source management (add, remove)
- Live UDP transport with actual FFmpeg source and capture
- SRT listener/caller transport
- Multi-output fan-out
- Source failover with primary kill and backup verification
- SRT encryption validation (valid config, short passphrase rejection, invalid algorithm rejection)
- MERGE mode validation (protocol compatibility, single-source graceful degradation)
- Source monitor configuration persistence
- Monitoring API endpoints (thumbnail, metadata, content quality) -- both offline and live
- Maintenance window configuration
- Enhanced metrics fields (uptime, per-source packets, per-output disconnections)
- SRT output encryption configuration
- Global events endpoint
- Event cleanup on flow deletion
- Double-stop idempotency
- Cache-Control headers

## License

See LICENSE file for details.
