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
	"syscall"
	"time"

	"github.com/openclaw/mroute/internal/merge"
	"github.com/openclaw/mroute/pkg/types"
)

// Engine manages FFmpeg-based live transport for flows
type Engine struct {
	ffmpegPath  string
	ffprobePath string
	maxFlows    int
	sessions    map[string]*Session
	mu          sync.Mutex
	onEvent     func(flowID, eventType, message, severity string)
	eventMu     sync.RWMutex
	nextMergePort int // allocator for merge output ports
}

// Session represents a running flow's transport session
type Session struct {
	FlowID        string
	Flow          *types.Flow
	ActiveSource  string
	Cmd           *exec.Cmd
	Cancel        context.CancelFunc
	Ctx           context.Context
	Done          chan struct{}
	Started       time.Time
	FailoverCount int64
	SourceHealth  map[string]*SourceHealth
	OutputHealth  map[string]*OutputHealth
	Merger        *merge.RTPMerger // non-nil in MERGE mode
	stopping      bool
	mu            sync.RWMutex
}

// SourceHealth tracks the liveness of a source
type SourceHealth struct {
	Name             string
	Connected        bool
	LastDataTime     time.Time
	BitrateKbps      int64
	PacketsReceived  int64
	PacketsLost      int64
	PacketsRecovered int64
	RoundTripTimeMs  int64
	JitterMs         int64
	Errors           int64
}

// OutputHealth tracks the status of an output
type OutputHealth struct {
	Name           string
	Connected      bool
	BitrateKbps    int64
	PacketsSent    int64
	Disconnections int64
}

func NewEngine(ffmpegPath string, maxFlows int) *Engine {
	if maxFlows <= 0 {
		maxFlows = 20
	}
	return &Engine{
		ffmpegPath:    ffmpegPath,
		ffprobePath:   "ffprobe",
		maxFlows:      maxFlows,
		sessions:      make(map[string]*Session),
		nextMergePort: 17000, // merge output ports start here
	}
}

func (e *Engine) SetFFprobePath(path string) {
	e.ffprobePath = path
}

func (e *Engine) SetEventCallback(cb func(flowID, eventType, message, severity string)) {
	e.eventMu.Lock()
	e.onEvent = cb
	e.eventMu.Unlock()
}

// allocMergePort returns a unique local port for merge output
func (e *Engine) allocMergePort() int {
	port := e.nextMergePort
	e.nextMergePort++
	return port
}

// StartFlow starts live transport for a flow.
func (e *Engine) StartFlow(flow *types.Flow) error {
	flowCopy := flow.DeepCopy()

	allSources := e.allSources(flowCopy)
	if len(allSources) == 0 {
		return fmt.Errorf("no source configured for flow %s", flowCopy.ID)
	}

	e.mu.Lock()
	if _, exists := e.sessions[flowCopy.ID]; exists {
		e.mu.Unlock()
		return fmt.Errorf("flow %s is already running", flowCopy.ID)
	}
	if len(e.sessions) >= e.maxFlows {
		e.mu.Unlock()
		return fmt.Errorf("maximum concurrent flows reached (%d)", e.maxFlows)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess := &Session{
		FlowID:       flowCopy.ID,
		Flow:         flowCopy,
		Cancel:       cancel,
		Ctx:          ctx,
		Done:         make(chan struct{}),
		Started:      time.Now(),
		SourceHealth: make(map[string]*SourceHealth),
		OutputHealth: make(map[string]*OutputHealth),
	}

	// Init health tracking
	for _, src := range allSources {
		sess.SourceHealth[src.Name] = &SourceHealth{Name: src.Name}
	}
	for _, out := range flowCopy.Outputs {
		sess.OutputHealth[out.Name] = &OutputHealth{Name: out.Name}
	}

	// Determine active source
	activeSource := e.pickActiveSource(flowCopy)
	if activeSource != nil {
		sess.ActiveSource = activeSource.Name
	}

	e.sessions[flowCopy.ID] = sess
	e.mu.Unlock()

	// Choose session mode based on failover config
	if e.isMergeMode(flowCopy) && len(allSources) >= 2 {
		go e.runMergeSession(sess)
	} else {
		go e.runFailoverSession(sess)
	}
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
	case <-time.After(15 * time.Second):
		log.Printf("[flow:%s] stop timed out", flowID)
		// Force cleanup if timed out (goroutine may be stuck)
		e.mu.Lock()
		delete(e.sessions, flowID)
		e.mu.Unlock()
	}

	return nil
}

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
		MergeActive:   sess.Merger != nil,
		Timestamp:     time.Now(),
	}

	for _, sh := range sess.SourceHealth {
		sm := &types.SourceMetrics{
			Name:             sh.Name,
			Connected:        sh.Connected,
			Status:           boolToStatus(sh.Connected),
			BitrateKbps:      sh.BitrateKbps,
			PacketsReceived:  sh.PacketsReceived,
			PacketsLost:      sh.PacketsLost,
			PacketsRecovered: sh.PacketsRecovered,
			RoundTripTimeMs:  sh.RoundTripTimeMs,
			JitterMs:         sh.JitterMs,
			SourceSelected:   sh.Name == sess.ActiveSource,
			MergeActive:      sess.Merger != nil,
		}
		m.SourceMetrics = append(m.SourceMetrics, sm)
	}

	for _, oh := range sess.OutputHealth {
		om := &types.OutputMetrics{
			Name:           oh.Name,
			Connected:      oh.Connected,
			Status:         boolToStatus(oh.Connected),
			BitrateKbps:    oh.BitrateKbps,
			PacketsSent:    oh.PacketsSent,
			Disconnections: oh.Disconnections,
		}
		m.OutputMetrics = append(m.OutputMetrics, om)
	}

	// Merge stats
	if sess.Merger != nil {
		stats := sess.Merger.Stats()
		m.MergeStats = &types.MergeMetrics{
			SourceAPackets: stats.SourceAPackets,
			SourceAGapFill: stats.SourceAUsed,
			SourceBPackets: stats.SourceBPackets,
			SourceBGapFill: stats.SourceBUsed,
			OutputPackets:  stats.OutputPackets,
			MissingPackets: stats.MissingPackets,
		}
	}

	return m
}

func (e *Engine) ListRunning() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		ids = append(ids, id)
	}
	return ids
}

func (e *Engine) StopAll() {
	e.mu.Lock()
	ids := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		ids = append(ids, id)
	}
	e.mu.Unlock()

	for _, id := range ids {
		if err := e.StopFlow(id); err != nil {
			log.Printf("[engine] StopAll: %s: %v", id, err)
		}
	}
}

// ===== MERGE MODE =====
// Runs both sources through RTPMerger, feeds merged stream to FFmpeg

func (e *Engine) runMergeSession(sess *Session) {
	defer func() {
		e.mu.Lock()
		delete(e.sessions, sess.FlowID)
		e.mu.Unlock()
		close(sess.Done)
	}()
	flowID := sess.FlowID

	sources := e.allSources(sess.Flow)
	if len(sources) < 2 {
		e.emit(flowID, "error", "MERGE mode requires 2 sources", "error")
		return
	}

	srcA := sources[0]
	srcB := sources[1]

	// Allocate a local port for merged output
	e.mu.Lock()
	mergePort := e.allocMergePort()
	e.mu.Unlock()

	recoveryMs := 100
	if sess.Flow.SourceFailoverConfig != nil && sess.Flow.SourceFailoverConfig.RecoveryWindow > 0 {
		recoveryMs = sess.Flow.SourceFailoverConfig.RecoveryWindow
	}

	merger := merge.NewRTPMerger(srcA.IngestPort, srcB.IngestPort, mergePort, recoveryMs)
	if err := merger.Start(); err != nil {
		e.emit(flowID, "error", fmt.Sprintf("Failed to start merger: %v", err), "error")
		return
	}

	sess.mu.Lock()
	sess.Merger = merger
	sess.ActiveSource = "merged(" + srcA.Name + "+" + srcB.Name + ")"
	sess.mu.Unlock()

	defer merger.Stop()

	e.emit(flowID, "merge_started", fmt.Sprintf("MERGE mode: %s (port %d) + %s (port %d) -> merged (port %d, recovery=%dms)",
		srcA.Name, srcA.IngestPort, srcB.Name, srcB.IngestPort, mergePort, recoveryMs), "info")

	// Create a synthetic source that reads from the merged local port
	mergedSource := &types.Source{
		Name:       "merged",
		Protocol:   types.ProtocolUDP, // merged output is raw UDP
		IngestPort: mergePort,
	}

	// Run FFmpeg reading from merged stream
	for {
		err := e.runFFmpeg(sess, mergedSource)
		if sess.Ctx.Err() != nil {
			e.emit(flowID, "flow_stopped", "Merge flow transport stopped", "info")
			return
		}

		if err != nil {
			e.emit(flowID, "merge_error", fmt.Sprintf("Merged stream error: %v", err), "warning")
		} else {
			e.emit(flowID, "merge_eof", "Merged stream ended", "warning")
		}

		// Update merge stats into source health
		stats := merger.Stats()
		sess.mu.Lock()
		if h, ok := sess.SourceHealth[srcA.Name]; ok {
			h.PacketsReceived = stats.SourceAPackets
			h.Connected = stats.SourceAPackets > 0
		}
		if h, ok := sess.SourceHealth[srcB.Name]; ok {
			h.PacketsReceived = stats.SourceBPackets
			h.Connected = stats.SourceBPackets > 0
		}
		sess.mu.Unlock()

		select {
		case <-time.After(2 * time.Second):
		case <-sess.Ctx.Done():
			return
		}
	}
}

// ===== FAILOVER MODE =====
// Standard failover with auto-recovery to primary

func (e *Engine) runFailoverSession(sess *Session) {
	defer func() {
		e.mu.Lock()
		delete(e.sessions, sess.FlowID)
		e.mu.Unlock()
		close(sess.Done)
	}()
	flowID := sess.FlowID

	// Start auto-recovery probe if configured with a primary source
	primaryName := e.getPrimarySourceName(sess.Flow)
	if primaryName != "" {
		go e.probeForPrimaryRecovery(sess, primaryName)
	}

	for {
		source := e.findSource(sess.Flow, sess.ActiveSource)
		if source == nil {
			e.emit(flowID, "error", "active source not found: "+sess.ActiveSource, "error")
			return
		}

		e.emit(flowID, "source_active", fmt.Sprintf("Using source: %s (%s on port %d)", source.Name, source.Protocol, source.IngestPort), "info")

		err := e.runFFmpeg(sess, source)

		if sess.Ctx.Err() != nil {
			e.emit(flowID, "flow_stopped", "Flow transport stopped", "info")
			return
		}

		if err != nil {
			e.emit(flowID, "source_error", fmt.Sprintf("Source %s failed: %v", source.Name, err), "warning")

			sess.mu.Lock()
			if h, ok := sess.SourceHealth[source.Name]; ok {
				h.Connected = false
				h.Errors++
			}
			sess.mu.Unlock()

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

					select {
					case <-time.After(200 * time.Millisecond):
					case <-sess.Ctx.Done():
						return
					}
					continue
				}
			}

			e.emit(flowID, "source_retry", fmt.Sprintf("Retrying source %s in 2s...", source.Name), "warning")
			select {
			case <-time.After(2 * time.Second):
			case <-sess.Ctx.Done():
				return
			}
			continue
		}

		// Clean exit - treat as failover trigger
		e.emit(flowID, "source_eof", fmt.Sprintf("Source %s ended (no more data)", source.Name), "warning")

		sess.mu.Lock()
		if h, ok := sess.SourceHealth[source.Name]; ok {
			h.Connected = false
		}
		sess.mu.Unlock()

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

				select {
				case <-time.After(200 * time.Millisecond):
				case <-sess.Ctx.Done():
					return
				}
				continue
			}
		}

		e.emit(flowID, "source_retry", fmt.Sprintf("Retrying source %s in 2s...", source.Name), "warning")
		select {
		case <-time.After(2 * time.Second):
		case <-sess.Ctx.Done():
			return
		}
	}
}

// probeForPrimaryRecovery runs in a goroutine during FAILOVER mode.
// When running on backup, it probes the primary source port and switches back when data arrives.
func (e *Engine) probeForPrimaryRecovery(sess *Session, primaryName string) {
	primarySource := e.findSource(sess.Flow, primaryName)
	if primarySource == nil {
		return
	}

	for {
		select {
		case <-sess.Ctx.Done():
			return
		case <-time.After(3 * time.Second): // probe every 3 seconds
		}

		sess.mu.RLock()
		currentActive := sess.ActiveSource
		sess.mu.RUnlock()

		// Only probe when we're NOT on the primary
		if currentActive == primaryName {
			continue
		}

		// Try to detect data on the primary source
		if e.probeSourceAlive(primarySource, 2*time.Second) {
			e.emit(sess.FlowID, "primary_recovered",
				fmt.Sprintf("Primary source %s recovered, switching back", primaryName), "info")

			sess.mu.Lock()
			sess.ActiveSource = primaryName
			sess.FailoverCount++
			sess.mu.Unlock()

			// The main loop will pick up the new ActiveSource on its next iteration
			// We need to interrupt the current FFmpeg to trigger the switch
			sess.mu.RLock()
			cmd := sess.Cmd
			sess.mu.RUnlock()
			if cmd != nil && cmd.Process != nil {
				cmd.Process.Signal(syscall.SIGTERM)
			}
		}
	}
}

// probeSourceAlive checks if a source is delivering data by running a quick ffprobe
func (e *Engine) probeSourceAlive(source *types.Source, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	uri := source.BuildSourceURI()
	cmd := exec.CommandContext(ctx, e.ffprobePath,
		"-v", "quiet",
		"-analyzeduration", "500000", // 500ms
		"-probesize", "32768",
		"-i", uri,
	)
	err := cmd.Run()
	return err == nil
}

// ===== FFmpeg Execution =====

func (e *Engine) runFFmpeg(sess *Session, source *types.Source) error {
	args := e.buildArgs(source, sess.Flow.Outputs)
	log.Printf("[flow:%s] ffmpeg %s", sess.FlowID, strings.Join(args, " "))

	cmd := exec.Command(e.ffmpegPath, args...)
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

	waitDone := make(chan struct{})
	go func() {
		select {
		case <-sess.Ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-waitDone:
				case <-time.After(5 * time.Second):
					cmd.Process.Kill()
				}
			}
		case <-waitDone:
		}
	}()

	e.monitorStderr(sess, source.Name, stderr)

	err = cmd.Wait()
	close(waitDone)

	if sess.Ctx.Err() != nil {
		return nil
	}
	return err
}

func (e *Engine) buildArgs(source *types.Source, outputs []*types.Output) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-stats",
	}

	args = append(args, e.sourceInputArgs(source)...)

	enabledOutputs := make([]*types.Output, 0)
	for _, out := range outputs {
		if out.Status != types.OutputDisabled {
			enabledOutputs = append(enabledOutputs, out)
		}
	}

	if len(enabledOutputs) == 0 {
		args = append(args, "-c", "copy", "-f", "null", "-")
		return args
	}

	for _, out := range enabledOutputs {
		args = append(args, "-c", "copy")
		args = append(args, e.outputArgs(out)...)
		args = append(args, out.BuildOutputURI())
	}

	return args
}

func (e *Engine) sourceInputArgs(source *types.Source) []string {
	var args []string

	switch source.Protocol {
	case types.ProtocolSRTListener, types.ProtocolSRTCaller:
		args = append(args, "-f", "mpegts")
	case types.ProtocolRTP, types.ProtocolRTPFEC:
		args = append(args, "-protocol_whitelist", "file,rtp,udp")
	case types.ProtocolUDP:
		args = append(args, "-buffer_size", "65536")
		args = append(args, "-timeout", "5000000")
	case types.ProtocolRIST:
		args = append(args, "-f", "mpegts")
	case types.ProtocolRTMP:
		args = append(args, "-live_start_index", "-1")
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

// ===== Monitoring =====

var progressRe = regexp.MustCompile(`frame=\s*\d+`)
var dropRe = regexp.MustCompile(`drop=\s*(\d+)`)
var dupRe = regexp.MustCompile(`dup=\s*(\d+)`)
var speedRe = regexp.MustCompile(`speed=\s*([\d.]+)x`)

func (e *Engine) monitorStderr(sess *Session, sourceName string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		log.Printf("[flow:%s] ffmpeg: %s", sess.FlowID, line)

		if progressRe.MatchString(line) {
			sess.mu.Lock()
			if h, ok := sess.SourceHealth[sourceName]; ok {
				h.Connected = true
				h.LastDataTime = time.Now()

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

				// Parse drop count
				if m := dropRe.FindStringSubmatch(line); len(m) > 1 {
					if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
						h.PacketsLost = v
					}
				}
			}

			// Mark all enabled outputs as connected when we see data flowing
			for _, out := range sess.Flow.Outputs {
				if out.Status != types.OutputDisabled {
					if oh, ok := sess.OutputHealth[out.Name]; ok {
						oh.Connected = true
					}
				}
			}
			sess.mu.Unlock()
		}

		lower := strings.ToLower(line)
		if strings.Contains(lower, "connection refused") ||
			strings.Contains(lower, "connection timed out") ||
			strings.Contains(lower, "no route to host") {
			e.emit(sess.FlowID, "connection_error", line, "error")
		}

		// Detect SRT-specific stats
		if strings.Contains(lower, "rtt=") {
			if idx := strings.Index(lower, "rtt="); idx >= 0 {
				rttStr := lower[idx+4:]
				if end := strings.IndexAny(rttStr, " \t,"); end > 0 {
					rttStr = rttStr[:end]
				}
				sess.mu.Lock()
				if h, ok := sess.SourceHealth[sourceName]; ok {
					if v, err := strconv.ParseFloat(rttStr, 64); err == nil {
						h.RoundTripTimeMs = int64(v)
					}
				}
				sess.mu.Unlock()
			}
		}
	}
}

// ===== Helpers =====

func (e *Engine) isMergeMode(flow *types.Flow) bool {
	if flow.SourceFailoverConfig == nil {
		return false
	}
	return flow.SourceFailoverConfig.State == types.FailoverEnabled &&
		flow.SourceFailoverConfig.FailoverMode == types.FailoverModeMerge
}

func (e *Engine) shouldFailover(flow *types.Flow) bool {
	if flow.SourceFailoverConfig == nil {
		return false
	}
	return flow.SourceFailoverConfig.State == types.FailoverEnabled && len(e.allSources(flow)) > 1
}

func (e *Engine) getPrimarySourceName(flow *types.Flow) string {
	if flow.SourceFailoverConfig != nil &&
		flow.SourceFailoverConfig.SourcePriority != nil {
		return flow.SourceFailoverConfig.SourcePriority.PrimarySource
	}
	return ""
}

func (e *Engine) pickActiveSource(flow *types.Flow) *types.Source {
	if flow.SourceFailoverConfig != nil &&
		flow.SourceFailoverConfig.SourcePriority != nil &&
		flow.SourceFailoverConfig.SourcePriority.PrimarySource != "" {
		for _, s := range e.allSources(flow) {
			if s.Name == flow.SourceFailoverConfig.SourcePriority.PrimarySource {
				return s
			}
		}
	}
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
