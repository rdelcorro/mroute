package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openclaw/mroute/pkg/types"
)

// Engine manages FFmpeg-based live transport for flows
type Engine struct {
	ffmpegPath string
	maxFlows   int
	sessions   map[string]*Session
	mu         sync.Mutex // use full Mutex (not RWMutex) for simpler concurrent safety
	onEvent    func(flowID, eventType, message, severity string)
	eventMu    sync.RWMutex // protects onEvent callback
}

// Session represents a running flow's transport session
type Session struct {
	FlowID        string
	Flow          *types.Flow
	ActiveSource  string // name of active source
	Cmd           *exec.Cmd
	Cancel        context.CancelFunc
	Ctx           context.Context
	Done          chan struct{}
	Started       time.Time
	FailoverCount int64
	SourceHealth  map[string]*SourceHealth
	stopping      bool // marks session as being stopped
	mu            sync.RWMutex
}

// SourceHealth tracks the liveness of a source
type SourceHealth struct {
	Name         string
	Connected    bool
	LastDataTime time.Time
	BitrateKbps  int64
	Errors       int64
}

func NewEngine(ffmpegPath string, maxFlows int) *Engine {
	if maxFlows <= 0 {
		maxFlows = 20
	}
	return &Engine{
		ffmpegPath: ffmpegPath,
		maxFlows:   maxFlows,
		sessions:   make(map[string]*Session),
	}
}

func (e *Engine) SetEventCallback(cb func(flowID, eventType, message, severity string)) {
	e.eventMu.Lock()
	e.onEvent = cb
	e.eventMu.Unlock()
}

// StartFlow starts live transport for a flow
func (e *Engine) StartFlow(flow *types.Flow) error {
	// Determine initial source before locking
	activeSource := e.pickActiveSource(flow)
	if activeSource == nil {
		return fmt.Errorf("no source configured for flow %s", flow.ID)
	}

	// Hold lock for entire check-and-insert to prevent TOCTOU race
	e.mu.Lock()
	if _, exists := e.sessions[flow.ID]; exists {
		e.mu.Unlock()
		return fmt.Errorf("flow %s is already running", flow.ID)
	}

	// Enforce MaxFlows limit
	if len(e.sessions) >= e.maxFlows {
		e.mu.Unlock()
		return fmt.Errorf("maximum concurrent flows reached (%d)", e.maxFlows)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess := &Session{
		FlowID:       flow.ID,
		Flow:         flow,
		ActiveSource: activeSource.Name,
		Cancel:       cancel,
		Ctx:          ctx,
		Done:         make(chan struct{}),
		Started:      time.Now(),
		SourceHealth: make(map[string]*SourceHealth),
	}

	// Init health tracking for all sources
	for _, src := range e.allSources(flow) {
		sess.SourceHealth[src.Name] = &SourceHealth{Name: src.Name}
	}

	e.sessions[flow.ID] = sess
	e.mu.Unlock()

	go e.runSession(sess)
	return nil
}

// StopFlow stops a running flow
func (e *Engine) StopFlow(flowID string) error {
	e.mu.Lock()
	sess, ok := e.sessions[flowID]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("flow %s is not running", flowID)
	}
	// Mark as stopping to prevent double-stop
	sess.mu.Lock()
	if sess.stopping {
		sess.mu.Unlock()
		e.mu.Unlock()
		return fmt.Errorf("flow %s is already stopping", flowID)
	}
	sess.stopping = true
	sess.mu.Unlock()
	e.mu.Unlock()

	sess.Cancel()

	select {
	case <-sess.Done:
	case <-time.After(10 * time.Second):
		log.Printf("[flow:%s] stop timed out, force killing", flowID)
		sess.mu.RLock()
		cmd := sess.Cmd
		sess.mu.RUnlock()
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
		}
	}

	e.mu.Lock()
	delete(e.sessions, flowID)
	e.mu.Unlock()

	return nil
}

// IsRunning returns whether a flow is actively transporting
func (e *Engine) IsRunning(flowID string) bool {
	e.mu.Lock()
	_, ok := e.sessions[flowID]
	e.mu.Unlock()
	return ok
}

// GetMetrics returns real-time metrics for a flow
func (e *Engine) GetMetrics(flowID string) *types.FlowMetrics {
	e.mu.Lock()
	sess, ok := e.sessions[flowID]
	e.mu.Unlock()
	if !ok {
		return nil
	}

	sess.mu.RLock()
	defer sess.mu.RUnlock()

	m := &types.FlowMetrics{
		FlowID:        flowID,
		ActiveSource:  sess.ActiveSource,
		Status:        types.FlowActive,
		UptimeSeconds: int64(time.Since(sess.Started).Seconds()),
		FailoverCount: sess.FailoverCount,
		Timestamp:     time.Now(),
	}

	for _, sh := range sess.SourceHealth {
		m.SourceMetrics = append(m.SourceMetrics, &types.SourceMetrics{
			Name:        sh.Name,
			Connected:   sh.Connected,
			Status:      boolToStatus(sh.Connected),
			BitrateKbps: sh.BitrateKbps,
		})
	}

	return m
}

// ListRunning returns IDs of all running flows
func (e *Engine) ListRunning() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		ids = append(ids, id)
	}
	return ids
}

// StopAll stops all running flows
func (e *Engine) StopAll() {
	e.mu.Lock()
	ids := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		ids = append(ids, id)
	}
	e.mu.Unlock()

	for _, id := range ids {
		e.StopFlow(id)
	}
}

// runSession is the main loop for a flow session.
// It runs FFmpeg, monitors health, and handles failover.
func (e *Engine) runSession(sess *Session) {
	defer close(sess.Done)
	flowID := sess.FlowID

	for {
		// Pick current active source
		source := e.findSource(sess.Flow, sess.ActiveSource)
		if source == nil {
			e.emit(flowID, "error", "active source not found: "+sess.ActiveSource, "error")
			return
		}

		e.emit(flowID, "source_active", fmt.Sprintf("Using source: %s (%s on port %d)", source.Name, source.Protocol, source.IngestPort), "info")

		// Build and run FFmpeg
		err := e.runFFmpeg(sess, source)

		// Check if we were cancelled (intentional stop)
		if sess.Ctx.Err() != nil {
			e.emit(flowID, "flow_stopped", "Flow transport stopped", "info")
			return
		}

		// FFmpeg exited unexpectedly - try failover
		if err != nil {
			e.emit(flowID, "source_error", fmt.Sprintf("Source %s failed: %v", source.Name, err), "warning")

			sess.mu.Lock()
			if h, ok := sess.SourceHealth[source.Name]; ok {
				h.Connected = false
				h.Errors++
			}
			sess.mu.Unlock()

			// Attempt failover if configured
			if e.shouldFailover(sess.Flow) {
				backup := e.getBackupSource(sess.Flow, source.Name)
				if backup != nil {
					sess.mu.Lock()
					sess.ActiveSource = backup.Name
					sess.FailoverCount++
					sess.mu.Unlock()

					e.emit(flowID, "source_failover",
						fmt.Sprintf("Failover: %s -> %s (count: %d)", source.Name, backup.Name, sess.FailoverCount),
						"warning")

					// Brief pause before retry with new source
					select {
					case <-time.After(200 * time.Millisecond):
					case <-sess.Ctx.Done():
						return
					}
					continue
				}
			}

			// No failover available - retry same source after delay
			e.emit(flowID, "source_retry", fmt.Sprintf("Retrying source %s in 2s...", source.Name), "warning")
			select {
			case <-time.After(2 * time.Second):
			case <-sess.Ctx.Done():
				return
			}
			continue
		}

		// Clean exit (shouldn't happen for live streams normally)
		e.emit(flowID, "source_eof", fmt.Sprintf("Source %s ended cleanly", source.Name), "info")
		return
	}
}

// runFFmpeg builds and runs the FFmpeg command for a given source -> outputs
func (e *Engine) runFFmpeg(sess *Session, source *types.Source) error {
	args := e.buildArgs(source, sess.Flow.Outputs)
	log.Printf("[flow:%s] ffmpeg %s", sess.FlowID, strings.Join(args, " "))

	cmd := exec.CommandContext(sess.Ctx, e.ffmpegPath, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	sess.mu.Lock()
	sess.Cmd = cmd
	sess.mu.Unlock()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	// Monitor stderr for health - Connected will be set when we see actual data
	e.monitorStderr(sess, source.Name, stderr)

	err = cmd.Wait()
	if sess.Ctx.Err() != nil {
		return nil // intentional cancellation
	}
	return err
}

// buildArgs constructs FFmpeg arguments for live transport (passthrough)
func (e *Engine) buildArgs(source *types.Source, outputs []*types.Output) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-stats",
	}

	// Source input args
	args = append(args, e.sourceInputArgs(source)...)

	// COPY - no transcoding, pure transport
	args = append(args, "-c", "copy")

	// Count enabled outputs
	enabledOutputs := make([]*types.Output, 0)
	for _, out := range outputs {
		if out.Status != types.OutputDisabled {
			enabledOutputs = append(enabledOutputs, out)
		}
	}

	// Guard: no enabled outputs - send to null
	if len(enabledOutputs) == 0 {
		args = append(args, "-f", "null", "-")
		return args
	}

	// Output args - each output is a separate destination
	if len(enabledOutputs) == 1 {
		out := enabledOutputs[0]
		args = append(args, e.outputArgs(out)...)
		args = append(args, out.BuildOutputURI())
	} else {
		// Multiple outputs: use tee muxer
		args = append(args, "-f", "tee")
		var teeTargets []string
		for _, out := range enabledOutputs {
			target := e.buildTeeTarget(out)
			teeTargets = append(teeTargets, target)
		}
		args = append(args, strings.Join(teeTargets, "|"))
	}

	return args
}

func (e *Engine) sourceInputArgs(source *types.Source) []string {
	var args []string

	switch source.Protocol {
	case types.ProtocolSRTListener, types.ProtocolSRTCaller:
		// SRT params (latency, maxbw) are in the URI
		// Input format for SRT is mpegts
		args = append(args, "-f", "mpegts")
	case types.ProtocolRTP, types.ProtocolRTPFEC:
		args = append(args, "-protocol_whitelist", "file,rtp,udp")
	case types.ProtocolUDP:
		// UDP: set reasonable buffer and timeout
		args = append(args, "-buffer_size", "65536")
		args = append(args, "-timeout", "5000000") // 5s timeout in microseconds
	case types.ProtocolRIST:
		// RIST: input format is mpegts
		args = append(args, "-f", "mpegts")
	case types.ProtocolRTMP:
		// RTMP listen mode - librtmp auto-detects FLV format
	}

	args = append(args, "-i", source.BuildSourceURI())
	return args
}

func (e *Engine) outputArgs(out *types.Output) []string {
	var args []string
	switch out.Protocol {
	case types.ProtocolRTP, types.ProtocolRTPFEC:
		args = append(args, "-f", "rtp")
	case types.ProtocolUDP:
		args = append(args, "-f", "mpegts")
	case types.ProtocolRTMP:
		args = append(args, "-f", "flv")
	case types.ProtocolSRTListener, types.ProtocolSRTCaller:
		args = append(args, "-f", "mpegts")
	case types.ProtocolRIST:
		args = append(args, "-f", "mpegts")
	}
	return args
}

func (e *Engine) buildTeeTarget(out *types.Output) string {
	uri := out.BuildOutputURI()
	switch out.Protocol {
	case types.ProtocolRTP, types.ProtocolRTPFEC:
		return "[f=rtp]" + uri
	case types.ProtocolUDP:
		return "[f=mpegts]" + uri
	case types.ProtocolRTMP:
		return "[f=flv]" + uri
	default:
		return "[f=mpegts]" + uri
	}
}

// Regex to detect FFmpeg progress lines like "frame= 123 fps= 25 ..."
var progressRe = regexp.MustCompile(`frame=\s*\d+`)

// monitorStderr reads FFmpeg stderr to detect connection health
func (e *Engine) monitorStderr(sess *Session, sourceName string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		log.Printf("[flow:%s] ffmpeg: %s", sess.FlowID, line)

		// Detect actual data flow from progress stats
		if progressRe.MatchString(line) {
			sess.mu.Lock()
			if h, ok := sess.SourceHealth[sourceName]; ok {
				h.Connected = true
				h.LastDataTime = time.Now()
				// Parse bitrate if available (e.g. "bitrate= 1234.5kbits/s")
				if idx := strings.Index(line, "bitrate="); idx >= 0 {
					br := strings.TrimSpace(line[idx+8:])
					if end := strings.IndexAny(br, " \t"); end > 0 {
						br = br[:end]
					}
					br = strings.TrimSuffix(br, "kbits/s")
					br = strings.TrimSpace(br)
					if v, err := strconv.ParseFloat(br, 64); err == nil {
						h.BitrateKbps = int64(v)
					}
				}
			}
			sess.mu.Unlock()
		}

		// Detect connection issues
		lower := strings.ToLower(line)
		if strings.Contains(lower, "connection refused") ||
			strings.Contains(lower, "connection timed out") ||
			strings.Contains(lower, "no route to host") {
			e.emit(sess.FlowID, "connection_error", line, "error")
		}
	}
}

// Failover helpers

func (e *Engine) shouldFailover(flow *types.Flow) bool {
	if flow.SourceFailoverConfig == nil {
		return false
	}
	return flow.SourceFailoverConfig.State == types.FailoverEnabled && len(e.allSources(flow)) > 1
}

func (e *Engine) pickActiveSource(flow *types.Flow) *types.Source {
	// If failover is configured with a primary, use it
	if flow.SourceFailoverConfig != nil &&
		flow.SourceFailoverConfig.SourcePriority != nil &&
		flow.SourceFailoverConfig.SourcePriority.PrimarySource != "" {
		for _, s := range e.allSources(flow) {
			if s.Name == flow.SourceFailoverConfig.SourcePriority.PrimarySource {
				return s
			}
		}
	}
	// Default to first source
	if flow.Source != nil {
		return flow.Source
	}
	if len(flow.Sources) > 0 {
		return flow.Sources[0]
	}
	return nil
}

func (e *Engine) getBackupSource(flow *types.Flow, currentName string) *types.Source {
	for _, s := range e.allSources(flow) {
		if s.Name != currentName {
			return s
		}
	}
	return nil
}

func (e *Engine) findSource(flow *types.Flow, name string) *types.Source {
	for _, s := range e.allSources(flow) {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func (e *Engine) allSources(flow *types.Flow) []*types.Source {
	var sources []*types.Source
	if flow.Source != nil {
		sources = append(sources, flow.Source)
	}
	sources = append(sources, flow.Sources...)
	return sources
}

func (e *Engine) emit(flowID, eventType, message, severity string) {
	log.Printf("[flow:%s] [%s] %s", flowID, eventType, message)
	e.eventMu.RLock()
	cb := e.onEvent
	e.eventMu.RUnlock()
	if cb != nil {
		cb(flowID, eventType, message, severity)
	}
}

func boolToStatus(b bool) string {
	if b {
		return "CONNECTED"
	}
	return "DISCONNECTED"
}
