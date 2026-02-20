# AGENT HANDOFF - Continuous Review & Fix Loop

**CRITICAL: READ THIS FILE COMPLETELY BEFORE DOING ANYTHING.**

You are an agent in a perpetual review loop for the `mroute` project. Your job:

1. **Review ALL code and ALL tests thoroughly**
2. **Fix every bug you find**
3. **Run `bash test_mroute.sh` and make EVERY test pass**
4. **When done, update this file with your findings, then tell the user to clear the session**

The user has explicitly said: **"I do not want this process to ever stop. I will decide when it's ready."**

---

## Project Overview

`mroute` is a live video transport engine written in Go. It uses FFmpeg for transport, supports UDP/SRT/RTP/RTMP/RIST protocols, source failover, SMPTE ST 2022-7 merge mode, SRT encryption, monitoring (thumbnails/metadata/content quality), and a REST API.

## Architecture (Relay-Based)

```
Source -> Input FFmpeg -> PacketRelay -> Per-output FFmpeg -> Destinations
                          (Go UDP)
```

- **Input FFmpeg**: Reads from source (UDP/SRT/RTP/etc), outputs mpegts to relay's input port
- **PacketRelay** (`relay.go`): Receives UDP packets, copies to all registered output ports
- **Per-output FFmpeg**: Each output has its own FFmpeg reading from a local relay port, writing to destination
- **Hot add/remove**: Adding/removing outputs only affects per-output FFmpeg processes; input and relay continue uninterrupted

## Key Files

| File | Purpose |
|------|---------|
| `internal/transport/engine.go` | Core engine: session management, input/output FFmpeg lifecycle, failover, merge mode |
| `internal/transport/relay.go` | PacketRelay: UDP fan-out between input and per-output FFmpeg processes |
| `internal/flow/manager.go` | Flow lifecycle, persistence (SQLite), hot add/remove wiring, maintenance windows |
| `internal/merge/merger.go` | SMPTE ST 2022-7 RTP merger for redundant streams |
| `internal/api/server.go` | REST API server |
| `internal/monitor/monitor.go` | Monitoring sidecar (thumbnails, metadata, content quality) |
| `internal/config/config.go` | YAML config loader |
| `internal/telegram/bot.go` | Telegram notifications |
| `pkg/types/types.go` | Core types, validation, URI builders |
| `cmd/mroute/main.go` | Entry point |
| `test_mroute.sh` | Integration test suite (real media validation) |

## KNOWN ISSUES (remaining)

### 1. restartInputIfRunning destroys relay and all output procs
Source changes (AddSource/RemoveSource) do a full stop/start of the engine session. This briefly interrupts ALL outputs. The relay and output FFmpeg processes are destroyed and recreated. For source changes this is acceptable (rare operation), but a future optimization could keep the relay and outputs running while only restarting the input FFmpeg.

### 2. Source failover recovery-to-primary not tested
The `probeForPrimaryRecovery` path (where the primary comes back and the system auto-switches back) is not covered by integration tests. It works in code but needs test validation.

### 3. MERGE single source test is timing-fragile
Test I starts a MERGE mode flow with only 1 source (port 5520). No data is sent to that port, so the engine falls back to FAILOVER mode. The test checks status=ACTIVE after 1 second. On slow systems this might fail if the session exits before the check.

### 4. Port allocation TOCTOU race
`allocOutputPort()` binds to port 0 to get a port, then releases it. Between release and actual use, another process could grab the port. This is inherent to the approach but rarely causes issues.

### 5. restartInputIfRunning TOCTOU (not serialized)
Between `StopFlow` and `StartFlow` in `restartInputIfRunning`, concurrent API calls (AddSource, RemoveSource, DeleteFlow, StartFlow) can race. This is unlikely in practice since source changes are rare admin operations, but a per-flow mutex would make it fully safe.

## COMPLETED FIXES

### Session 5 (2026-02-19) - Topology Change Coverage + Output Death Recovery

**All 95 tests pass. 0 failures. Real media validated (H.264/AAC via ffprobe).**

**Code fixes (2):**

1. **engine.go: probeSourceAlive missing Setpgid** - The recovery probe's ffprobe process did not set `Setpgid: true`, unlike all other process-spawning functions (input FFmpeg, output FFmpeg, monitor processes). Any child processes spawned by ffprobe during probing would not be killed on context cancellation. Now consistent with engine process management.

2. **engine.go: Dead output FFmpeg goes undetected (CRITICAL)** - When a per-output FFmpeg process dies unexpectedly (e.g., downstream rejects connection), nobody noticed. The relay kept sending UDP packets to a dead port (silently dropped), the output appeared healthy in metrics, and no events were emitted. Added `watchOutputProc` goroutine that:
   - Detects output FFmpeg death via `proc.Done` channel
   - Distinguishes unexpected death from deliberate removal (checks `OutputProcs` map)
   - Emits `output_died` event and marks health as disconnected
   - Cleans up dead proc from relay and OutputProcs map
   - Attempts restart with exponential backoff (2s, 4s, 8s) up to 3 times
   - Emits `output_restarted` or `output_restart_failed`/`output_restart_limit` events
   - Watcher is started for ALL output FFmpeg processes: initial outputs in `StartFlow`, hot-added outputs in `AddOutputToRunning`, and restarted outputs recursively

**New topology change tests (14 new tests, 81→95):**

3. **D.6: Remove PRIMARY source while running (5 tests)** - Tests removing the primary source from a running flow (not just the backup like D.5). The backup is promoted to primary, flow restarts via `restartInputIfRunning`. Validates: API succeeds (200), flow stays ACTIVE, media resumes on the backup source with valid H.264/AAC (290KB capture).

4. **D.7: UpdateOutput hot-swap while running (5 tests)** - Tests `PUT /v1/flows/{id}/outputs/{name}` to change an output's port while the flow is running. The engine hot-swaps the output (remove old + add new). Validates: API succeeds (200), port is updated in response, media appears on new port with valid H.264/AAC (327KB), old port no longer receives data (0 bytes).

5. **D.8: Start with zero outputs, then hot-add (4 tests)** - Tests the edge case of starting a flow with no outputs, then hot-adding one. The relay and input run without any outputs, then AddOutputToRunning wires up the new output to the relay. Validates: flow creates with 0 outputs (201), flow goes ACTIVE, hot-add API succeeds (200), output receives valid H.264/AAC (332KB).

### Session 4 (2026-02-19) - Hardening Fixes + Test Gaps

**All 81 tests pass. 0 failures. Real media validated (H.264/AAC via ffprobe).**

**Code fixes (4):**

1. **relay.go: AddOutput double-close race with Stop()** - The old connection was closed but the entry remained in the map, allowing Stop() to double-close it. Now the entry is deleted from the map before releasing the lock, preventing Stop() from seeing a stale entry with a closed connection.

2. **engine.go: Dead output FFmpeg not detected during startup** - `startOutputProc` used a blind 300ms sleep before registering with the relay. If FFmpeg died during startup (e.g., bad output URI), the dead process was registered as active. Now uses `select` on `proc.Done` vs `time.After(300ms)` to detect early death and return an error.

3. **engine.go: probeSourceAlive used context.Background()** - The recovery probe's `ffprobe` process was not cancelled when the session context was cancelled, causing up to 2s cleanup delay on flow stop. Now uses the session context as parent, so the probe is killed immediately when the session stops.

4. **monitor.go: Missing Setpgid on monitor processes** - Monitor FFmpeg/ffprobe processes did not set `Setpgid: true`, unlike engine processes. Any child processes spawned by FFmpeg during monitoring would not be killed on context cancellation. Now consistent with engine process management.

**Test improvements (6 new tests, 75→81):**

5. **Hot output add continuity proof** - Captures from existing output CONTINUOUSLY through the add operation (10s capture spanning the add). Proves hitless: 849KB of uninterrupted H.264/AAC with valid 320x240 resolution.

6. **Hot output remove continuity proof** - Same technique: captures from remaining output THROUGH the remove operation. Proves hitless: 875KB of uninterrupted media.

7. **D.5: Hot source remove from running flow (4 tests)** - Previously only tested source removal on stopped flows. Now tests `RemoveSource` on a running flow which triggers `restartInputIfRunning` - validates API succeeds, flow stays ACTIVE, and media resumes (324KB valid H.264/AAC after restart).

8. **Failover post-switch quality fix** - Previous test showed `h264/aac@0x0(15980b)` after failover (0x0 resolution, 15KB - garbage). Added 5s stabilization wait and raised min capture to 20KB. Now shows `h264/aac@320x240(495192b)` - proper resolution, 495KB of real media.

### Session 3 (2026-02-19) - Critical Code Review Fixes

**All 75 tests pass. 0 failures. Real media validated (H.264/AAC via ffprobe).**

Fixes applied based on thorough code review:

1. **relay.go: Connection leak on name collision** - `AddOutput()` now closes old connection before replacing. Prevents leaking UDP connections when the same output name is re-added (e.g., `__monitor`).

2. **relay.go: Stop/AddOutput race condition** - Added `stopped` flag protected by mutex. `AddOutput()` returns error if relay is stopped. `Stop()` sets flag and closes all outputs under the lock, preventing new outputs from leaking.

3. **relay.go: Optimized run loop** - Moved stop channel check to timeout path only, reducing per-packet overhead.

4. **engine.go: Monitor relay output leak** - `GetMonitorURI()` now tracks the `__monitor` output and reuses it on repeated calls. Added `RemoveMonitorURI()` called by manager on flow stop to clean up the relay tap.

5. **engine.go: TOCTOU race in AddOutputToRunning** - Duplicate check now happens atomically under write lock inside `startOutputProc()`.

6. **engine.go: sess.Cmd set after Start()** - Input FFmpeg's `sess.Cmd` is now set AFTER `cmd.Start()` succeeds, preventing recovery probe from reading a command with nil Process handle.

7. **engine.go: sess.Cmd cleared after process exit** - `runInputFFmpeg` now clears `sess.Cmd = nil` after the process exits, preventing stale process handles in the recovery probe.

8. **engine.go: Output FFmpeg gets SIGTERM instead of SIGKILL** - Output FFmpeg processes now use `cmd.Cancel` to send SIGTERM for graceful shutdown with 5-second WaitDelay fallback to SIGKILL.

9. **engine.go: FFmpeg process groups** - Both input and output FFmpeg processes set `Setpgid: true` so child processes are properly cleaned up. Force-kill uses `syscall.Kill(-pid, SIGKILL)` to kill the entire process group.

10. **engine.go: stopOutputProc waits for kill** - After force-killing an output FFmpeg, the function now waits for `proc.Done` instead of returning immediately, ensuring the process is fully reclaimed.

11. **engine.go: Output FFmpeg uses URL params for UDP options** - Moved `-buffer_size` and `-timeout` into the UDP URL parameters. Added explicit `-f mpegts` for faster format detection.

12. **engine.go: Session cleanup handles __monitor_meta** - Defer blocks in both `runMergeSession` and `runFailoverSession` properly clean up the `__monitor_meta` tracking entry.

13. **merger.go: Reduced lock contention** - `outputLoop` now processes packets in bounded batches (max 64) and updates stats outside the buffer lock. Prune step runs periodically (every ~100 ticks) instead of every iteration.

14. **manager.go: Double-close protection** - `Manager.Close()` uses `sync.Once` to prevent panic on double-close of the stopMW channel.

15. **manager.go: Monitor relay cleanup on stop** - `StopFlow` now calls `engine.RemoveMonitorURI()` to clean up the relay tap before stopping the flow.

16. **test_mroute.sh: Fixed test count bug** - Removed redundant `TOTAL` increments in sections P that caused 77 total vs 75 pass discrepancy. Now correctly shows 75/75.

### Previous Sessions

1. **Data race in engine.go**: `sess.ActiveSource` now read under `sess.mu.RLock()` in failover loop
2. **Double FailoverCount**: When recovery probe changes source, main loop detects external change and skips manual failover
3. **Goroutine leak in merger.go**: Added `recvWg sync.WaitGroup` for receive goroutines
4. **macOS compatibility**: Test uses `wc -c` instead of `stat -c%s`
5. **Relay architecture**: Created `relay.go`, rewrote `engine.go` to use input/relay/output separation
6. **Hot output add/remove**: `manager.go` calls `engine.AddOutputToRunning`/`RemoveOutputFromRunning` instead of restarting
7. **Test pre-cleanup**: Test kills leftover processes on startup

## WHAT YOU MUST DO

### Step 1: Read all source files
Read every file listed in "Key Files" above. Understand the full codebase.

### Step 2: Run the full test suite
```bash
bash test_mroute.sh 2>&1 | tee last_test_run.log
```

### Step 3: Fix every failure
Go through each FAIL and fix the root cause. Don't just adjust expected values - fix the actual code.

### Step 4: Investigate remaining known issues
- Can `restartInputIfRunning` be improved to only restart input while keeping relay+outputs?
- Add a test for primary recovery (recovery probe switches back to primary)
- Strengthen the MERGE single source test

### Step 5: Look for new issues
- Race conditions
- Resource leaks
- Edge cases
- Missing test coverage
- Performance bottlenecks

### Step 6: Update this file
Replace findings, add new issues discovered, update LAST RUN RESULTS.

### Step 7: Tell the user
Tell the user: "Review complete. X tests pass, Y fail. [summary of changes]. Clear session with /clear to continue the loop."

## LAST RUN RESULTS

**Date**: 2026-02-19 (Session 5)
**Result**: ALL PASS
**Tests passed**: 95/95 (0 failed, 0 skipped)
**Real media validation**: H.264/AAC content verified by ffprobe in all transport tests
**Features tested**: UDP transport, multi-output fan-out, hot output add (with continuity proof), hot output remove (with continuity proof), hot source add with failover, hot source remove while running, **remove primary source while running**, **UpdateOutput hot-swap (port change) while running**, **start with zero outputs then hot-add**, SRT listener, SRT encrypted, encryption validation, source failover (with proper post-switch media validation), MERGE mode, monitoring (thumbnails/metadata/content quality), events, maintenance windows, cache headers, double-stop idempotency, metrics
**Fixes this session**: 2 code fixes + 14 new tests (probeSourceAlive Setpgid, output death detection + restart watcher, remove primary source topology test, UpdateOutput hot-swap topology test, zero-output start + hot-add topology test)

## PREVIOUS AGENT NOTES

- The `restartIfRunning` approach was rejected by the user because it stops playback
- The relay architecture was designed to solve hitless output management
- The user explicitly does NOT want any FFmpeg process stopped when adding/removing outputs
- DO NOT ASK THE USER QUESTIONS - just fix everything and report results
