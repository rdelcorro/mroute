# CLAUDE.md - Project Instructions

## MANDATORY FIRST ACTION

**Before doing ANYTHING else, read `/home/openclaw/mroute/AGENT_HANDOFF.md` completely.**

That file contains the full review loop instructions, known bugs, architecture details, and what you must do.

## Quick Context

This is `mroute` - a live video transport engine (like AWS Elemental MediaConnect). Go backend, FFmpeg-based transport, REST API.

## Architecture

Input FFmpeg → PacketRelay (Go UDP fan-out) → Per-output FFmpeg → Destinations

This enables hitless output add/remove without stopping the input stream.

## Build & Test

```bash
go build ./cmd/mroute/         # build
go vet ./...                   # lint
bash test_mroute.sh            # full integration test suite (real media)
```

## Perpetual Review Loop

You are part of a continuous improvement loop. The user has said:
> "I do not want this process to ever stop. I will decide when it's ready."

Your job every session:
1. Read AGENT_HANDOFF.md
2. Review all code
3. Fix all bugs
4. Run tests until all pass
5. Update AGENT_HANDOFF.md with findings
6. Tell the user to clear the session to continue the loop

DO NOT ASK QUESTIONS. Just fix everything and report results.
