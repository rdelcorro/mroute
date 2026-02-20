package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
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

// Engine manages FFmpeg-based live transport for flows.
// Architecture: Input FFmpeg → PacketRelay → Per-output FFmpeg processes.
// This enables hitless output add/remove without interrupting the input stream.
type Engine struct {
	ffmpegPath  string
	ffprobePath string
	maxFlows    int
	sessions    map[string]*Session
	mu          sync.Mutex
	onEvent     func(flowID, eventType, message, severity string)
	eventMu     sync.RWMutex
}

// OutputProc represents a running per-output FFmpeg process
type OutputProc struct {
	Name      string
	Output    *types.Output
	Cmd       *exec.Cmd
	Cancel    context.CancelFunc
	LocalPort int
	Done      chan struct{}
}

// Session represents a running flow's transport session
type Session struct {
	FlowID        string
	Flow          *types.Flow
	ActiveSource  string
	Cmd           *exec.Cmd // input FFmpeg command (for signalling on failover)
	Cancel        context.CancelFunc
	Ctx           context.Context
	Done          chan struct{}
	Started       time.Time
	FailoverCount int64
	SourceHealth  map[string]*SourceHealth
	OutputHealth  map[string]*OutputHealth
	Merger        *merge.RTPMerger // non-nil in MERGE mode
	Relay         *PacketRelay     // fan-out relay between input and outputs
	OutputProcs   map[string]*OutputProc
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
		ffmpegPath:  ffmpegPath,
		ffprobePath: "ffprobe",
		maxFlows:    maxFlows,
		sessions:    make(map[string]*Session),
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

// GetMonitorURI adds a monitoring tap to a running flow's relay and returns a URI to read from.
// Returns empty string if the flow is not running or has no relay.
func (e *Engine) GetMonitorURI(flowID string) string {
	e.mu.Lock()
	sess, ok := e.sessions[flowID]
	e.mu.Unlock()
	if !ok || sess.Relay == nil {
		return ""
	}

	// Reuse existing monitor output if already registered
	if sess.Relay.HasOutput("__monitor") {
		sess.mu.RLock()
		proc, exists := sess.OutputProcs["__monitor_meta"]
		sess.mu.RUnlock()
		if exists {
			return fmt.Sprintf("udp://127.0.0.1:%d", proc.LocalPort)
		}
	}

	port, err := e.allocOutputPort()
	if err != nil {
		log.Printf("[flow:%s] failed to alloc monitor port: %v", flowID, err)
		return ""
	}

	if err := sess.Relay.AddOutput("__monitor", port); err != nil {
		log.Printf("[flow:%s] failed to add monitor relay output: %v", flowID, err)
		return ""
	}

	// Track the port so we can return it on subsequent calls
	sess.mu.Lock()
	sess.OutputProcs["__monitor_meta"] = &OutputProc{
		Name:      "__monitor_meta",
		LocalPort: port,
		Done:      make(chan struct{}),
	}
	sess.mu.Unlock()

	return fmt.Sprintf("udp://127.0.0.1:%d", port)
}

// RemoveMonitorURI removes the monitoring tap from a running flow's relay.
func (e *Engine) RemoveMonitorURI(flowID string) {
	e.mu.Lock()
	sess, ok := e.sessions[flowID]
	e.mu.Unlock()
	if !ok || sess.Relay == nil {
		return
	}
	sess.Relay.RemoveOutput("__monitor")
	sess.mu.Lock()
	delete(sess.OutputProcs, "__monitor_meta")
	sess.mu.Unlock()
}

// allocMergePort returns a free local UDP port for merge output
func (e *Engine) allocMergePort() (int, error) {
	return e.allocOutputPort()
}

// allocOutputPort returns a free local UDP port by binding to port 0 and releasing.
func (e *Engine) allocOutputPort() (int, error) {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("resolve UDP addr: %w", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return 0, fmt.Errorf("listen UDP: %w", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port, nil
}

// StartFlow starts live transport for a flow.
// Creates a PacketRelay, starts per-output FFmpeg processes, then starts the input FFmpeg.
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
		OutputProcs:  make(map[string]*OutputProc),
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

	// Create and start packet relay
	relay := NewPacketRelay()
	if err := relay.Start(); err != nil {
		cancel()
		e.mu.Unlock()
		return fmt.Errorf("start relay: %w", err)
	}
	sess.Relay = relay

	e.sessions[flowCopy.ID] = sess
	e.mu.Unlock()

	// Start per-output FFmpeg processes (relay fans out to each)
	for _, out := range flowCopy.Outputs {
		if out.Status == types.OutputDisabled {
			continue
		}
		proc, err := e.startOutputProc(sess, out)
		if err != nil {
			log.Printf("[flow:%s] Failed to start output %s: %v", flowCopy.ID, out.Name, err)
			continue
		}
		go e.watchOutputProc(sess, proc, 0)
	}

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

// ===== PER-OUTPUT FFmpeg MANAGEMENT =====

// startOutputProc starts a per-output FFmpeg process and registers it in the relay.
// The output FFmpeg reads from a local UDP port and writes to the output destination.
func (e *Engine) startOutputProc(sess *Session, output *types.Output) (*OutputProc, error) {
	localPort, err := e.allocOutputPort()
	if err != nil {
		return nil, fmt.Errorf("alloc output port: %w", err)
	}

	args := e.buildOutputArgs(localPort, output)
	log.Printf("[flow:%s] output %s: ffmpeg %s", sess.FlowID, output.Name, strings.Join(args, " "))

	ctx, cancel := context.WithCancel(sess.Ctx)
	cmd := exec.CommandContext(ctx, e.ffmpegPath, args...)
	// Use process group so child processes are also killed on cancel
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Send SIGTERM instead of SIGKILL for graceful shutdown
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start output ffmpeg: %w", err)
	}

	proc := &OutputProc{
		Name:      output.Name,
		Output:    output,
		Cmd:       cmd,
		Cancel:    cancel,
		LocalPort: localPort,
		Done:      make(chan struct{}),
	}

	go func() {
		defer close(proc.Done)
		e.monitorOutputStderr(sess, output.Name, stderr)
		cmd.Wait()
	}()

	// Atomically check for duplicate and insert
	sess.mu.Lock()
	if _, exists := sess.OutputProcs[output.Name]; exists {
		sess.mu.Unlock()
		cancel()
		<-proc.Done
		return nil, fmt.Errorf("output %s already running (race)", output.Name)
	}
	sess.OutputProcs[output.Name] = proc
	sess.mu.Unlock()

	// Brief pause for FFmpeg to bind its UDP listener, then register in relay
	select {
	case <-proc.Done:
		// Process died during startup - clean up and report error
		sess.mu.Lock()
		delete(sess.OutputProcs, output.Name)
		sess.mu.Unlock()
		return nil, fmt.Errorf("output ffmpeg exited during startup")
	case <-time.After(300 * time.Millisecond):
	}

	if err := sess.Relay.AddOutput(output.Name, localPort); err != nil {
		cancel()
		<-proc.Done
		sess.mu.Lock()
		delete(sess.OutputProcs, output.Name)
		sess.mu.Unlock()
		return nil, fmt.Errorf("add to relay: %w", err)
	}

	return proc, nil
}

// stopOutputProc removes an output from the relay and stops its FFmpeg process
func (e *Engine) stopOutputProc(sess *Session, name string) {
	sess.mu.Lock()
	proc, ok := sess.OutputProcs[name]
	if !ok {
		sess.mu.Unlock()
		return
	}
	delete(sess.OutputProcs, name)
	sess.mu.Unlock()

	// Remove from relay first (stop sending packets)
	if sess.Relay != nil {
		sess.Relay.RemoveOutput(name)
	}

	// Then stop the FFmpeg process
	proc.Cancel()
	select {
	case <-proc.Done:
	case <-time.After(5 * time.Second):
		if proc.Cmd.Process != nil {
			syscall.Kill(-proc.Cmd.Process.Pid, syscall.SIGKILL)
		}
		<-proc.Done
	}
}

const maxOutputRestarts = 3

// watchOutputProc monitors an output FFmpeg process and restarts it if it dies unexpectedly.
// It detects death via proc.Done, emits events, updates health, and attempts restart with
// exponential backoff (2s, 4s, 8s) up to maxOutputRestarts times.
func (e *Engine) watchOutputProc(sess *Session, proc *OutputProc, restartCount int) {
	select {
	case <-proc.Done:
	case <-sess.Ctx.Done():
		return
	}

	// Session shutting down - expected death, don't restart
	if sess.Ctx.Err() != nil {
		return
	}

	// Check if this was a deliberate removal (stopOutputProc deletes from map before killing)
	sess.mu.RLock()
	_, stillExists := sess.OutputProcs[proc.Name]
	sess.mu.RUnlock()
	if !stillExists {
		return
	}

	e.emit(sess.FlowID, "output_died",
		fmt.Sprintf("Output %s FFmpeg exited unexpectedly", proc.Name), "warning")

	// Mark as disconnected
	sess.mu.Lock()
	if oh, ok := sess.OutputHealth[proc.Name]; ok {
		oh.Connected = false
		oh.Disconnections++
	}
	sess.mu.Unlock()

	// Clean up dead proc from relay and map
	if sess.Relay != nil {
		sess.Relay.RemoveOutput(proc.Name)
	}
	sess.mu.Lock()
	delete(sess.OutputProcs, proc.Name)
	sess.mu.Unlock()

	if restartCount >= maxOutputRestarts {
		e.emit(sess.FlowID, "output_restart_limit",
			fmt.Sprintf("Output %s exceeded restart limit (%d), giving up", proc.Name, maxOutputRestarts),
			"error")
		return
	}

	// Exponential backoff: 2s, 4s, 8s
	delay := time.Duration(2<<uint(restartCount)) * time.Second
	select {
	case <-time.After(delay):
	case <-sess.Ctx.Done():
		return
	}

	if sess.Ctx.Err() != nil {
		return
	}

	newProc, err := e.startOutputProc(sess, proc.Output)
	if err != nil {
		e.emit(sess.FlowID, "output_restart_failed",
			fmt.Sprintf("Failed to restart output %s: %v", proc.Name, err), "error")
		return
	}

	e.emit(sess.FlowID, "output_restarted",
		fmt.Sprintf("Output %s restarted (attempt %d/%d)", proc.Name, restartCount+1, maxOutputRestarts),
		"info")

	go e.watchOutputProc(sess, newProc, restartCount+1)
}

// AddOutputToRunning hot-adds an output to a running flow without interrupting other outputs or the input
func (e *Engine) AddOutputToRunning(flowID string, output *types.Output) error {
	e.mu.Lock()
	sess, ok := e.sessions[flowID]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("flow %s is not running", flowID)
	}

	// startOutputProc does the duplicate check atomically under write lock
	proc, err := e.startOutputProc(sess, output)
	if err != nil {
		return err
	}

	sess.mu.Lock()
	sess.OutputHealth[output.Name] = &OutputHealth{Name: output.Name}
	sess.mu.Unlock()

	go e.watchOutputProc(sess, proc, 0)

	e.emit(flowID, "output_added", fmt.Sprintf("Hot-added output %s (relay port %d)", output.Name, proc.LocalPort), "info")
	return nil
}

// RemoveOutputFromRunning hot-removes an output from a running flow without interrupting other outputs or the input
func (e *Engine) RemoveOutputFromRunning(flowID string, outputName string) error {
	e.mu.Lock()
	sess, ok := e.sessions[flowID]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("flow %s is not running", flowID)
	}

	e.stopOutputProc(sess, outputName)

	sess.mu.Lock()
	delete(sess.OutputHealth, outputName)
	sess.mu.Unlock()

	e.emit(flowID, "output_removed", fmt.Sprintf("Hot-removed output %s", outputName), "info")
	return nil
}

// ===== MERGE MODE =====

func (e *Engine) runMergeSession(sess *Session) {
	defer func() {
		// Stop all output procs (including __monitor_meta tracking entry)
		sess.mu.RLock()
		names := make([]string, 0, len(sess.OutputProcs))
		for name := range sess.OutputProcs {
			names = append(names, name)
		}
		sess.mu.RUnlock()
		for _, name := range names {
			if name == "__monitor_meta" {
				// Just remove the tracking entry, relay.Stop handles the connection
				sess.mu.Lock()
				delete(sess.OutputProcs, name)
				sess.mu.Unlock()
				continue
			}
			e.stopOutputProc(sess, name)
		}
		if sess.Relay != nil {
			sess.Relay.Stop()
		}

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

	mergePort, err := e.allocMergePort()
	if err != nil {
		e.emit(flowID, "error", fmt.Sprintf("Failed to alloc merge port: %v", err), "error")
		return
	}

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

	mergedSource := &types.Source{
		Name:       "merged",
		Protocol:   types.ProtocolUDP,
		IngestPort: mergePort,
	}

	for {
		err := e.runInputFFmpeg(sess, mergedSource)
		if sess.Ctx.Err() != nil {
			e.emit(flowID, "flow_stopped", "Merge flow transport stopped", "info")
			return
		}

		if err != nil {
			e.emit(flowID, "merge_error", fmt.Sprintf("Merged stream error: %v", err), "warning")
		} else {
			e.emit(flowID, "merge_eof", "Merged stream ended", "warning")
		}

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

func (e *Engine) runFailoverSession(sess *Session) {
	defer func() {
		// Stop all output procs (including __monitor_meta tracking entry)
		sess.mu.RLock()
		names := make([]string, 0, len(sess.OutputProcs))
		for name := range sess.OutputProcs {
			names = append(names, name)
		}
		sess.mu.RUnlock()
		for _, name := range names {
			if name == "__monitor_meta" {
				sess.mu.Lock()
				delete(sess.OutputProcs, name)
				sess.mu.Unlock()
				continue
			}
			e.stopOutputProc(sess, name)
		}
		if sess.Relay != nil {
			sess.Relay.Stop()
		}

		e.mu.Lock()
		delete(e.sessions, sess.FlowID)
		e.mu.Unlock()
		close(sess.Done)
	}()
	flowID := sess.FlowID

	primaryName := e.getPrimarySourceName(sess.Flow)
	if primaryName != "" {
		go e.probeForPrimaryRecovery(sess, primaryName)
	}

	for {
		sess.mu.RLock()
		activeName := sess.ActiveSource
		sess.mu.RUnlock()

		source := e.findSource(sess.Flow, activeName)
		if source == nil {
			e.emit(flowID, "error", "active source not found: "+activeName, "error")
			return
		}

		e.emit(flowID, "source_active", fmt.Sprintf("Using source: %s (%s on port %d)", source.Name, source.Protocol, source.IngestPort), "info")

		err := e.runInputFFmpeg(sess, source)

		if sess.Ctx.Err() != nil {
			e.emit(flowID, "flow_stopped", "Flow transport stopped", "info")
			return
		}

		// Check if active source was changed externally (e.g., by recovery probe)
		sess.mu.RLock()
		currentActive := sess.ActiveSource
		sess.mu.RUnlock()
		if currentActive != activeName {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-sess.Ctx.Done():
				return
			}
			continue
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
		case <-time.After(3 * time.Second):
		}

		sess.mu.RLock()
		currentActive := sess.ActiveSource
		sess.mu.RUnlock()

		if currentActive == primaryName {
			continue
		}

		if e.probeSourceAlive(sess.Ctx, primarySource, 2*time.Second) {
			e.emit(sess.FlowID, "primary_recovered",
				fmt.Sprintf("Primary source %s recovered, switching back", primaryName), "info")

			sess.mu.Lock()
			sess.ActiveSource = primaryName
			sess.FailoverCount++
			cmd := sess.Cmd
			sess.mu.Unlock()

			// Interrupt the current input FFmpeg to trigger the switch
			if cmd != nil && cmd.Process != nil {
				cmd.Process.Signal(syscall.SIGTERM)
			}
		}
	}
}

// probeSourceAlive checks if a source is delivering data by running a quick ffprobe
func (e *Engine) probeSourceAlive(parent context.Context, source *types.Source, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	uri := source.BuildSourceURI()
	cmd := exec.CommandContext(ctx, e.ffprobePath,
		"-v", "quiet",
		"-analyzeduration", "500000",
		"-probesize", "32768",
		"-i", uri,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := cmd.Run()
	return err == nil
}

// ===== INPUT FFmpeg =====

// runInputFFmpeg runs the input-side FFmpeg: reads from source, writes to relay's input port
func (e *Engine) runInputFFmpeg(sess *Session, source *types.Source) error {
	args := e.buildInputArgs(source, sess.Relay.InputPort())
	log.Printf("[flow:%s] input ffmpeg %s", sess.FlowID, strings.Join(args, " "))

	cmd := exec.Command(e.ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	// Set sess.Cmd AFTER Start so the process handle is valid
	sess.mu.Lock()
	sess.Cmd = cmd
	sess.mu.Unlock()

	waitDone := make(chan struct{})
	go func() {
		select {
		case <-sess.Ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-waitDone:
				case <-time.After(5 * time.Second):
					syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
			}
		case <-waitDone:
		}
	}()

	e.monitorStderr(sess, source.Name, stderr)

	err = cmd.Wait()
	close(waitDone)

	// Clear sess.Cmd after process exits
	sess.mu.Lock()
	if sess.Cmd == cmd {
		sess.Cmd = nil
	}
	sess.mu.Unlock()

	if sess.Ctx.Err() != nil {
		return nil
	}
	return err
}

// buildInputArgs constructs FFmpeg args for reading from a source and writing to the relay
func (e *Engine) buildInputArgs(source *types.Source, relayPort int) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-nostats",
		"-fflags", "+genpts+discardcorrupt",
	}

	args = append(args, e.sourceInputArgs(source)...)

	// Output to relay's input port as mpegts over UDP
	args = append(args, "-c", "copy", "-f", "mpegts",
		fmt.Sprintf("udp://127.0.0.1:%d?pkt_size=1316", relayPort))

	return args
}

// buildOutputArgs constructs FFmpeg args for reading from a local relay port and writing to an output.
func (e *Engine) buildOutputArgs(localPort int, output *types.Output) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-nostats",
		"-fflags", "+genpts+discardcorrupt",
		"-probesize", "32768",
		"-analyzeduration", "2000000",
		"-f", "mpegts",
		"-i", fmt.Sprintf("udp://127.0.0.1:%d?timeout=30000000&fifo_size=131072&overrun_nonfatal=1&buffer_size=65536", localPort),
		"-c", "copy",
	}

	args = append(args, e.outputArgs(output)...)
	args = append(args, output.BuildOutputURI())

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
		args = append(args, "-f", "mpegts")
		args = append(args, "-probesize", "5000000")
		args = append(args, "-analyzeduration", "5000000")
		args = append(args, "-buffer_size", "65536")
		args = append(args, "-timeout", "10000000") // 10s read timeout for source loss detection
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

				if m := dropRe.FindStringSubmatch(line); len(m) > 1 {
					if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
						h.PacketsLost = v
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

func (e *Engine) monitorOutputStderr(sess *Session, outputName string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		log.Printf("[flow:%s] output %s: %s", sess.FlowID, outputName, line)

		if progressRe.MatchString(line) {
			sess.mu.Lock()
			if oh, ok := sess.OutputHealth[outputName]; ok {
				oh.Connected = true
			}
			sess.mu.Unlock()
		}

		lower := strings.ToLower(line)
		if strings.Contains(lower, "connection refused") ||
			strings.Contains(lower, "connection timed out") ||
			strings.Contains(lower, "no route to host") {
			sess.mu.Lock()
			if oh, ok := sess.OutputHealth[outputName]; ok {
				oh.Connected = false
				oh.Disconnections++
			}
			sess.mu.Unlock()
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
