package monitor

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openclaw/mroute/pkg/types"
)

// Monitor runs sidecar processes for thumbnails, metadata, and content quality
type Monitor struct {
	ffmpegPath  string
	ffprobePath string
	sessions    map[string]*MonitorSession
	mu          sync.Mutex
}

// MonitorSession tracks sidecar processes for a single flow
type MonitorSession struct {
	flowID     string
	sourceURI  string
	cancel     context.CancelFunc
	ctx        context.Context
	done       chan struct{}

	thumbnail     string // base64-encoded JPEG
	thumbnailTime string
	thumbMu       sync.RWMutex

	metadata   *types.SourceMetadata
	metaMu     sync.RWMutex

	quality    *types.ContentQuality
	qualityMu  sync.RWMutex
}

func NewMonitor(ffmpegPath, ffprobePath string) *Monitor {
	return &Monitor{
		ffmpegPath:  ffmpegPath,
		ffprobePath: ffprobePath,
		sessions:    make(map[string]*MonitorSession),
	}
}

// StartMonitoring begins sidecar processes for a flow
func (m *Monitor) StartMonitoring(flowID, sourceURI string, cfg *types.SourceMonitorConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[flowID]; exists {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess := &MonitorSession{
		flowID:    flowID,
		sourceURI: sourceURI,
		cancel:    cancel,
		ctx:       ctx,
		done:      make(chan struct{}),
	}

	m.sessions[flowID] = sess

	thumbnailEnabled := cfg != nil && cfg.ThumbnailEnabled
	qualityEnabled := cfg != nil && cfg.ContentQualityEnabled
	interval := 1
	if cfg != nil && cfg.ThumbnailIntervalSec > 0 {
		interval = cfg.ThumbnailIntervalSec
	}

	var wg sync.WaitGroup

	// Always run metadata probe
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.probeMetadata(sess)
	}()

	if thumbnailEnabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.runThumbnailCapture(sess, interval)
		}()
	}
	if qualityEnabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.runContentQuality(sess)
		}()
	}

	go func() {
		wg.Wait()
		close(sess.done)
	}()

	log.Printf("[monitor:%s] Started (thumbnail=%v quality=%v)", flowID, thumbnailEnabled, qualityEnabled)
}

// StopMonitoring halts sidecar processes for a flow
func (m *Monitor) StopMonitoring(flowID string) {
	m.mu.Lock()
	sess, ok := m.sessions[flowID]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, flowID)
	m.mu.Unlock()

	sess.cancel()
	select {
	case <-sess.done:
	case <-time.After(5 * time.Second):
	}
}

// StopAll stops all monitoring sessions
func (m *Monitor) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		m.StopMonitoring(id)
	}
}

// GetThumbnail returns the latest thumbnail for a flow
func (m *Monitor) GetThumbnail(flowID string) *types.Thumbnail {
	m.mu.Lock()
	sess, ok := m.sessions[flowID]
	m.mu.Unlock()
	if !ok {
		return nil
	}

	sess.thumbMu.RLock()
	defer sess.thumbMu.RUnlock()

	if sess.thumbnail == "" {
		return nil
	}
	return &types.Thumbnail{
		FlowID:    flowID,
		Data:      sess.thumbnail,
		Timestamp: sess.thumbnailTime,
	}
}

// GetMetadata returns the source metadata for a flow
func (m *Monitor) GetMetadata(flowID string) *types.SourceMetadata {
	m.mu.Lock()
	sess, ok := m.sessions[flowID]
	m.mu.Unlock()
	if !ok {
		return nil
	}

	sess.metaMu.RLock()
	defer sess.metaMu.RUnlock()
	return sess.metadata
}

// GetContentQuality returns the content quality status for a flow
func (m *Monitor) GetContentQuality(flowID string) *types.ContentQuality {
	m.mu.Lock()
	sess, ok := m.sessions[flowID]
	m.mu.Unlock()
	if !ok {
		return nil
	}

	sess.qualityMu.RLock()
	defer sess.qualityMu.RUnlock()
	if sess.quality == nil {
		return nil
	}
	cp := *sess.quality
	return &cp
}

// probeMetadata runs ffprobe to extract transport stream metadata
func (m *Monitor) probeMetadata(sess *MonitorSession) {
	// Wait a bit for the stream to start
	select {
	case <-time.After(3 * time.Second):
	case <-sess.ctx.Done():
		return
	}

	for {
		select {
		case <-sess.ctx.Done():
			return
		default:
		}

		args := []string{
			"-v", "quiet",
			"-print_format", "json",
			"-show_streams",
			"-show_programs",
			"-fflags", "+genpts+discardcorrupt",
			"-analyzeduration", "3000000",
			"-probesize", "3000000",
			"-f", "mpegts",
			sess.sourceURI,
		}

		ctx, cancel := context.WithTimeout(sess.ctx, 10*time.Second)
		cmd := exec.CommandContext(ctx, m.ffprobePath, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out, err := cmd.Output()
		cancel()

		if err != nil {
			log.Printf("[monitor:%s] ffprobe error: %v", sess.flowID, err)
			select {
			case <-time.After(10 * time.Second):
			case <-sess.ctx.Done():
				return
			}
			continue
		}

		meta := parseFFprobeOutput(sess.flowID, out)
		sess.metaMu.Lock()
		sess.metadata = meta
		sess.metaMu.Unlock()

		// Re-probe every 30 seconds
		select {
		case <-time.After(30 * time.Second):
		case <-sess.ctx.Done():
			return
		}
	}
}

// runThumbnailCapture captures JPEG thumbnails from the source
func (m *Monitor) runThumbnailCapture(sess *MonitorSession, intervalSec int) {
	select {
	case <-time.After(5 * time.Second):
	case <-sess.ctx.Done():
		return
	}

	tmpFile := fmt.Sprintf("/tmp/mroute_thumb_%s.jpg", sess.flowID)
	defer os.Remove(tmpFile)

	for {
		select {
		case <-sess.ctx.Done():
			return
		default:
		}

		args := []string{
			"-hide_banner",
			"-loglevel", "error",
			"-fflags", "+genpts+discardcorrupt",
			"-f", "mpegts",
			"-i", sess.sourceURI,
			"-vframes", "1",
			"-q:v", "5",
			"-y",
			tmpFile,
		}

		ctx, cancel := context.WithTimeout(sess.ctx, 10*time.Second)
		cmd := exec.CommandContext(ctx, m.ffmpegPath, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err := cmd.Run()
		cancel()

		if err == nil {
			data, err := os.ReadFile(tmpFile)
			if err == nil && len(data) > 0 {
				b64 := base64.StdEncoding.EncodeToString(data)
				sess.thumbMu.Lock()
				sess.thumbnail = b64
				sess.thumbnailTime = time.Now().Format(time.RFC3339)
				sess.thumbMu.Unlock()
			}
		}

		select {
		case <-time.After(time.Duration(intervalSec) * time.Second):
		case <-sess.ctx.Done():
			return
		}
	}
}

// runContentQuality uses FFmpeg filters to detect black frames, frozen frames, and silence
func (m *Monitor) runContentQuality(sess *MonitorSession) {
	select {
	case <-time.After(5 * time.Second):
	case <-sess.ctx.Done():
		return
	}

	for {
		select {
		case <-sess.ctx.Done():
			return
		default:
		}

		// Run a short analysis with detection filters
		args := []string{
			"-hide_banner",
			"-fflags", "+genpts+discardcorrupt",
			"-f", "mpegts",
			"-i", sess.sourceURI,
			"-t", "5", // analyze 5 seconds
			"-vf", "blackdetect=d=0.5:pix_th=0.10,freezedetect=n=0.003:d=1",
			"-af", "silencedetect=n=-50dB:d=2",
			"-f", "null", "-",
		}

		ctx, cancel := context.WithTimeout(sess.ctx, 15*time.Second)
		cmd := exec.CommandContext(ctx, m.ffmpegPath, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			cancel()
			select {
			case <-time.After(10 * time.Second):
			case <-sess.ctx.Done():
				return
			}
			continue
		}

		if err := cmd.Start(); err != nil {
			cancel()
			log.Printf("[monitor:%s] content quality ffmpeg start error: %v", sess.flowID, err)
			select {
			case <-time.After(10 * time.Second):
			case <-sess.ctx.Done():
				return
			}
			continue
		}
		quality := parseQualityOutput(stderr)
		cmd.Wait()
		cancel()

		sess.qualityMu.Lock()
		sess.quality = quality
		sess.quality.LastCheckTime = time.Now().Format(time.RFC3339)
		sess.qualityMu.Unlock()

		// Re-analyze every 10 seconds
		select {
		case <-time.After(10 * time.Second):
		case <-sess.ctx.Done():
			return
		}
	}
}

// parseFFprobeOutput parses ffprobe JSON output into SourceMetadata
func parseFFprobeOutput(flowID string, data []byte) *types.SourceMetadata {
	var result struct {
		Programs []struct {
			ProgramID  int    `json:"program_id"`
			Tags       struct {
				ServiceName string `json:"service_name"`
			} `json:"tags"`
		} `json:"programs"`
		Streams []struct {
			Index          int    `json:"index"`
			CodecType      string `json:"codec_type"`
			CodecName      string `json:"codec_name"`
			Profile        string `json:"profile"`
			Width          int    `json:"width"`
			Height         int    `json:"height"`
			RFrameRate     string `json:"r_frame_rate"`
			SampleRate     string `json:"sample_rate"`
			Channels       int    `json:"channels"`
			BitRate        string `json:"bit_rate"`
			ID             string `json:"id"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(data, &result); err != nil {
		log.Printf("[monitor:%s] ffprobe parse error: %v", flowID, err)
		return nil
	}

	meta := &types.SourceMetadata{FlowID: flowID}

	for _, p := range result.Programs {
		meta.Programs = append(meta.Programs, &types.ProgramInfo{
			ProgramID:   p.ProgramID,
			ProgramName: p.Tags.ServiceName,
		})
	}

	for _, s := range result.Streams {
		si := &types.StreamInfo{
			Index:   s.Index,
			Codec:   s.CodecName,
			Profile: s.Profile,
		}

		switch s.CodecType {
		case "video":
			si.StreamType = "video"
			si.Width = s.Width
			si.Height = s.Height
			si.FrameRate = s.RFrameRate
		case "audio":
			si.StreamType = "audio"
			si.Channels = s.Channels
			if sr, err := fmt.Sscanf(s.SampleRate, "%d", &si.SampleRate); sr == 0 || err != nil {
				si.SampleRate = 0
			}
		case "data":
			si.StreamType = "data"
			if strings.Contains(strings.ToLower(s.CodecName), "scte") {
				si.Codec = "SCTE35"
			}
		default:
			si.StreamType = s.CodecType
		}

		if s.BitRate != "" {
			fmt.Sscanf(s.BitRate, "%d", &si.BitRate)
		}

		meta.Streams = append(meta.Streams, si)
	}

	return meta
}

// parseQualityOutput reads FFmpeg stderr for detection filter output
func parseQualityOutput(r io.Reader) *types.ContentQuality {
	q := &types.ContentQuality{
		VideoStreamPresent: true, // assume present until proven otherwise
		AudioStreamPresent: true,
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.ToLower(scanner.Text())

		if strings.Contains(line, "black_start") {
			q.BlackFrameDetected = true
		}
		if strings.Contains(line, "freeze_start") || strings.Contains(line, "lavfi.freezedetect") {
			q.FrozenFrameDetected = true
		}
		if strings.Contains(line, "silence_start") {
			q.SilentAudioDetected = true
		}
		if strings.Contains(line, "no video stream") || strings.Contains(line, "video:0kb") {
			q.VideoStreamPresent = false
		}
		if strings.Contains(line, "no audio stream") || strings.Contains(line, "audio:0kb") {
			q.AudioStreamPresent = false
		}
	}

	return q
}
