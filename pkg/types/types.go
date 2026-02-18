package types

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Protocol represents a supported transport protocol
type Protocol string

const (
	ProtocolSRTListener Protocol = "srt-listener"
	ProtocolSRTCaller   Protocol = "srt-caller"
	ProtocolRTP         Protocol = "rtp"
	ProtocolRTPFEC      Protocol = "rtp-fec"
	ProtocolRIST        Protocol = "rist"
	ProtocolUDP         Protocol = "udp"
	ProtocolRTMP        Protocol = "rtmp"
)

// ValidProtocols lists all supported protocols
var ValidProtocols = map[Protocol]bool{
	ProtocolSRTListener: true,
	ProtocolSRTCaller:   true,
	ProtocolRTP:         true,
	ProtocolRTPFEC:      true,
	ProtocolRIST:        true,
	ProtocolUDP:         true,
	ProtocolRTMP:        true,
}

// MergeCapableProtocols can use MERGE mode (RTP-based sequence numbers)
var MergeCapableProtocols = map[Protocol]bool{
	ProtocolRTP:    true,
	ProtocolRTPFEC: true,
	ProtocolRIST:   true,
}

// IsValidProtocol checks if a protocol is supported
func IsValidProtocol(p Protocol) bool {
	return ValidProtocols[p]
}

// CanMerge checks if a protocol supports MERGE mode
func CanMerge(p Protocol) bool {
	return MergeCapableProtocols[p]
}

// FlowStatus represents the state of a flow
type FlowStatus string

const (
	FlowStandby  FlowStatus = "STANDBY"
	FlowActive   FlowStatus = "ACTIVE"
	FlowStarting FlowStatus = "STARTING"
	FlowStopping FlowStatus = "STOPPING"
	FlowError    FlowStatus = "ERROR"
)

// FailoverMode represents how source failover operates
type FailoverMode string

const (
	FailoverModeFailover FailoverMode = "FAILOVER"
	FailoverModeMerge    FailoverMode = "MERGE"
)

// FailoverState is whether failover is enabled
type FailoverState string

const (
	FailoverEnabled  FailoverState = "ENABLED"
	FailoverDisabled FailoverState = "DISABLED"
)

// OutputStatus for individual outputs
type OutputStatus string

const (
	OutputEnabled  OutputStatus = "ENABLED"
	OutputDisabled OutputStatus = "DISABLED"
)

// SourceStatus for source health
type SourceStatus string

const (
	SourceConnected    SourceStatus = "CONNECTED"
	SourceDisconnected SourceStatus = "DISCONNECTED"
	SourceError        SourceStatus = "ERROR"
)

// Flow is the core entity - a live transport path from source(s) to output(s)
type Flow struct {
	ID                   string               `json:"id"`
	Name                 string               `json:"name"`
	Description          string               `json:"description,omitempty"`
	Status               FlowStatus           `json:"status"`
	Source               *Source              `json:"source"`
	Sources              []*Source            `json:"sources,omitempty"`
	Outputs              []*Output            `json:"outputs"`
	SourceFailoverConfig *FailoverConfig      `json:"source_failover_config,omitempty"`
	SourceMonitorConfig  *SourceMonitorConfig `json:"source_monitor_config,omitempty"`
	MaintenanceWindow    *MaintenanceWindow   `json:"maintenance_window,omitempty"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
	StartedAt            *time.Time           `json:"started_at,omitempty"`
	StoppedAt            *time.Time           `json:"stopped_at,omitempty"`
}

// SourceMonitorConfig controls monitoring features for a flow
type SourceMonitorConfig struct {
	ThumbnailEnabled       bool `json:"thumbnail_enabled,omitempty"`
	ContentQualityEnabled  bool `json:"content_quality_enabled,omitempty"`
	ThumbnailIntervalSec   int  `json:"thumbnail_interval_sec,omitempty"`   // default 1
}

// MaintenanceWindow defines when a flow can be restarted for maintenance
type MaintenanceWindow struct {
	DayOfWeek string `json:"day_of_week"` // Monday, Tuesday, etc.
	StartHour int    `json:"start_hour"`  // 0-23 UTC
}

// Source represents an ingest source for a flow
type Source struct {
	Name              string       `json:"name"`
	Protocol          Protocol     `json:"protocol"`
	IngestPort        int          `json:"ingest_port,omitempty"`
	Description       string       `json:"description,omitempty"`
	WhitelistCidr     string       `json:"whitelist_cidr,omitempty"`
	SenderIPAddress   string       `json:"sender_ip_address,omitempty"`
	SenderControlPort int          `json:"sender_control_port,omitempty"`
	MaxBitrate        int          `json:"max_bitrate,omitempty"`  // bytes/sec for SRT maxbw
	MaxLatency        int          `json:"max_latency,omitempty"`  // milliseconds
	MinLatency        int          `json:"min_latency,omitempty"`  // milliseconds
	StreamID          string       `json:"stream_id,omitempty"`
	Decryption        *Encryption  `json:"decryption,omitempty"`   // SRT decryption
	Status            SourceStatus `json:"status"`
	IngestIP          string       `json:"ingest_ip,omitempty"`
}

// Output represents a destination for a flow
type Output struct {
	Name             string       `json:"name"`
	Protocol         Protocol     `json:"protocol"`
	Destination      string       `json:"destination"`
	Port             int          `json:"port"`
	Description      string       `json:"description,omitempty"`
	MaxLatency       int          `json:"max_latency,omitempty"` // milliseconds
	MinLatency       int          `json:"min_latency,omitempty"` // milliseconds
	SmoothingLatency int          `json:"smoothing_latency,omitempty"`
	StreamKey        string       `json:"stream_key,omitempty"` // for RTMP output
	CidrAllowList    []string     `json:"cidr_allow_list,omitempty"`
	Encryption       *Encryption  `json:"encryption,omitempty"` // SRT encryption
	Status           OutputStatus `json:"status"`
}

// Encryption holds SRT passphrase encryption settings
type Encryption struct {
	Algorithm  string `json:"algorithm,omitempty"`  // aes128, aes192, aes256
	Passphrase string `json:"passphrase,omitempty"` // 10-79 characters
}

// FailoverConfig controls source redundancy behavior
type FailoverConfig struct {
	State          FailoverState   `json:"state"`
	FailoverMode   FailoverMode    `json:"failover_mode"`
	RecoveryWindow int             `json:"recovery_window,omitempty"` // milliseconds
	SourcePriority *SourcePriority `json:"source_priority,omitempty"`
}

// SourcePriority designates the primary source in FAILOVER mode
type SourcePriority struct {
	PrimarySource string `json:"primary_source"`
}

// FlowMetrics holds real-time metrics for a running flow
type FlowMetrics struct {
	FlowID         string           `json:"flow_id"`
	ActiveSource   string           `json:"active_source"`
	Status         FlowStatus       `json:"status"`
	UptimeSeconds  int64            `json:"uptime_seconds"`
	SourceMetrics  []*SourceMetrics `json:"source_metrics"`
	OutputMetrics  []*OutputMetrics `json:"output_metrics"`
	FailoverCount  int64            `json:"failover_count"`
	MergeActive    bool             `json:"merge_active,omitempty"`
	MergeStats     *MergeMetrics    `json:"merge_stats,omitempty"`
	ContentQuality *ContentQuality  `json:"content_quality,omitempty"`
	Timestamp      time.Time        `json:"timestamp"`
}

// MergeMetrics holds statistics for MERGE mode operation
type MergeMetrics struct {
	SourceAPackets int64 `json:"source_a_packets"`
	SourceAGapFill int64 `json:"source_a_gap_fills"`
	SourceBPackets int64 `json:"source_b_packets"`
	SourceBGapFill int64 `json:"source_b_gap_fills"`
	OutputPackets  int64 `json:"output_packets"`
	MissingPackets int64 `json:"missing_packets"` // gaps not filled by either source
}

// SourceMetrics holds metrics for a single source
type SourceMetrics struct {
	Name                  string `json:"name"`
	Status                string `json:"status"`
	Connected             bool   `json:"connected"`
	BitrateKbps           int64  `json:"bitrate_kbps"`
	PacketsReceived       int64  `json:"packets_received"`
	PacketsLost           int64  `json:"packets_lost"`
	PacketsRecovered      int64  `json:"packets_recovered"`
	RoundTripTimeMs       int64  `json:"round_trip_time_ms"`
	JitterMs              int64  `json:"jitter_ms"`
	SourceSelected        bool   `json:"source_selected,omitempty"`  // true if this is the active source
	MergeActive           bool   `json:"merge_active,omitempty"`     // true if participating in merge
}

// OutputMetrics holds metrics for a single output
type OutputMetrics struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	Connected       bool   `json:"connected"`
	BitrateKbps     int64  `json:"bitrate_kbps"`
	PacketsSent     int64  `json:"packets_sent"`
	Disconnections  int64  `json:"disconnections"`
}

// ContentQuality holds content quality detection results
type ContentQuality struct {
	BlackFrameDetected  bool    `json:"black_frame_detected"`
	FrozenFrameDetected bool    `json:"frozen_frame_detected"`
	SilentAudioDetected bool    `json:"silent_audio_detected"`
	VideoStreamPresent  bool    `json:"video_stream_present"`
	AudioStreamPresent  bool    `json:"audio_stream_present"`
	LastCheckTime       string  `json:"last_check_time,omitempty"`
}

// SourceMetadata holds parsed transport stream metadata from ffprobe
type SourceMetadata struct {
	FlowID   string            `json:"flow_id"`
	Programs []*ProgramInfo    `json:"programs"`
	Streams  []*StreamInfo     `json:"streams"`
}

// ProgramInfo describes a transport stream program
type ProgramInfo struct {
	ProgramID   int    `json:"program_id"`
	ProgramName string `json:"program_name,omitempty"`
}

// StreamInfo describes a single stream within the transport
type StreamInfo struct {
	Index       int    `json:"index"`
	StreamType  string `json:"stream_type"` // video, audio, data
	Codec       string `json:"codec"`
	Profile     string `json:"profile,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	FrameRate   string `json:"frame_rate,omitempty"`
	SampleRate  int    `json:"sample_rate,omitempty"`
	Channels    int    `json:"channels,omitempty"`
	BitRate     int64  `json:"bit_rate,omitempty"`
	PID         int    `json:"pid,omitempty"`
}

// Thumbnail holds a captured frame from the source
type Thumbnail struct {
	FlowID    string `json:"flow_id"`
	Data      string `json:"data"`      // base64-encoded JPEG
	Timestamp string `json:"timestamp"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// Event represents something that happened in the system
type Event struct {
	ID        string                 `json:"id"`
	FlowID    string                 `json:"flow_id"`
	Type      string                 `json:"type"`
	Message   string                 `json:"message"`
	Severity  string                 `json:"severity"` // info, warning, error
	Timestamp time.Time              `json:"timestamp"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// ValidateSource validates a source's fields
func ValidateSource(s *Source) error {
	if s.Name == "" {
		return fmt.Errorf("source name required")
	}
	if !IsValidProtocol(s.Protocol) {
		return fmt.Errorf("unsupported source protocol: %s", s.Protocol)
	}
	// SRT-caller needs sender address and port instead of ingest port
	if s.Protocol == ProtocolSRTCaller {
		if s.SenderIPAddress == "" {
			return fmt.Errorf("source %s: sender_ip_address required for srt-caller", s.Name)
		}
		if net.ParseIP(s.SenderIPAddress) == nil {
			return fmt.Errorf("source %s: invalid sender_ip_address (must be valid IP)", s.Name)
		}
		if s.SenderControlPort < 1 || s.SenderControlPort > 65535 {
			return fmt.Errorf("source %s: invalid sender_control_port %d (must be 1-65535)", s.Name, s.SenderControlPort)
		}
	} else {
		// Listener protocols need a valid ingest port
		if s.IngestPort < 1 || s.IngestPort > 65535 {
			return fmt.Errorf("source %s: invalid ingest_port %d (must be 1-65535)", s.Name, s.IngestPort)
		}
	}
	// Validate StreamID
	if s.StreamID != "" && strings.ContainsAny(s.StreamID, " \t\n\r?&#") {
		return fmt.Errorf("source %s: stream_id must not contain whitespace or special URI characters", s.Name)
	}
	// Validate SRT encryption
	if s.Decryption != nil {
		if err := validateEncryption(s.Decryption); err != nil {
			return fmt.Errorf("source %s decryption: %w", s.Name, err)
		}
	}
	return nil
}

// ValidateOutput validates an output's fields
func ValidateOutput(o *Output) error {
	if o.Name == "" {
		return fmt.Errorf("output name required")
	}
	if !IsValidProtocol(o.Protocol) {
		return fmt.Errorf("unsupported output protocol: %s", o.Protocol)
	}
	if o.Port < 1 || o.Port > 65535 {
		return fmt.Errorf("output %s: invalid port %d (must be 1-65535)", o.Name, o.Port)
	}
	// Non-listener outputs need a valid destination
	if o.Protocol != ProtocolSRTListener {
		if o.Destination == "" {
			return fmt.Errorf("output %s: destination required for protocol %s", o.Name, o.Protocol)
		}
		if err := validateHost(o.Destination); err != nil {
			return fmt.Errorf("output %s: invalid destination: %w", o.Name, err)
		}
	}
	// Validate StreamKey
	if o.StreamKey != "" && strings.ContainsAny(o.StreamKey, " \t\n\r?&#/") {
		return fmt.Errorf("output %s: stream_key must not contain whitespace or special URI characters", o.Name)
	}
	// Validate SRT encryption
	if o.Encryption != nil {
		if err := validateEncryption(o.Encryption); err != nil {
			return fmt.Errorf("output %s encryption: %w", o.Name, err)
		}
	}
	return nil
}

// ValidateFailoverConfig checks failover config consistency.
// Source count is not checked here since sources can be added after flow creation;
// the engine validates source count at start time.
func ValidateFailoverConfig(fc *FailoverConfig, sources []*Source) error {
	if fc == nil || fc.State != FailoverEnabled {
		return nil
	}
	if fc.FailoverMode == FailoverModeMerge {
		// MERGE mode requires all sources to use merge-capable protocols
		for _, s := range sources {
			if !CanMerge(s.Protocol) {
				return fmt.Errorf("MERGE mode not supported for protocol %s (source %s); use RTP, RTP-FEC, or RIST", s.Protocol, s.Name)
			}
		}
	}
	return nil
}

func validateEncryption(e *Encryption) error {
	if e.Passphrase == "" {
		return fmt.Errorf("passphrase required")
	}
	if len(e.Passphrase) < 10 || len(e.Passphrase) > 79 {
		return fmt.Errorf("passphrase must be 10-79 characters")
	}
	if e.Algorithm != "" {
		switch e.Algorithm {
		case "aes128", "aes192", "aes256":
		default:
			return fmt.Errorf("algorithm must be aes128, aes192, or aes256")
		}
	}
	return nil
}

func validateHost(h string) error {
	if net.ParseIP(h) != nil {
		return nil
	}
	if strings.ContainsAny(h, " \t\n\r/?#:@") {
		return fmt.Errorf("contains invalid characters")
	}
	if h == "" || len(h) > 253 {
		return fmt.Errorf("empty or too long")
	}
	return nil
}

// BuildSourceURI constructs an FFmpeg-compatible URI for a source
func (s *Source) BuildSourceURI() string {
	switch s.Protocol {
	case ProtocolSRTListener:
		uri := "srt://0.0.0.0:" + strconv.Itoa(s.IngestPort) + "?mode=listener"
		if s.MaxLatency > 0 {
			uri += "&latency=" + strconv.Itoa(s.MaxLatency)
		}
		if s.MaxBitrate > 0 {
			uri += "&maxbw=" + strconv.Itoa(s.MaxBitrate)
		}
		uri += srtEncryptionParams(s.Decryption)
		return uri
	case ProtocolSRTCaller:
		uri := "srt://" + s.SenderIPAddress + ":" + strconv.Itoa(s.SenderControlPort) + "?mode=caller"
		if s.MaxLatency > 0 {
			uri += "&latency=" + strconv.Itoa(s.MaxLatency)
		}
		if s.MaxBitrate > 0 {
			uri += "&maxbw=" + strconv.Itoa(s.MaxBitrate)
		}
		if s.StreamID != "" {
			uri += "&streamid=" + url.QueryEscape(s.StreamID)
		}
		uri += srtEncryptionParams(s.Decryption)
		return uri
	case ProtocolRTP:
		return "rtp://" + listenAddr(s.IngestPort)
	case ProtocolRTPFEC:
		return "rtp://" + listenAddr(s.IngestPort)
	case ProtocolRIST:
		return "rist://" + listenAddr(s.IngestPort)
	case ProtocolUDP:
		return "udp://" + listenAddr(s.IngestPort)
	case ProtocolRTMP:
		streamKey := s.StreamID
		if streamKey == "" {
			streamKey = "stream"
		}
		return "rtmp://0.0.0.0:" + strconv.Itoa(s.IngestPort) + "/live/" + streamKey
	default:
		return ""
	}
}

// BuildOutputURI constructs an FFmpeg-compatible URI for an output
func (o *Output) BuildOutputURI() string {
	switch o.Protocol {
	case ProtocolSRTListener:
		uri := "srt://0.0.0.0:" + strconv.Itoa(o.Port) + "?mode=listener"
		if o.MaxLatency > 0 {
			uri += "&latency=" + strconv.Itoa(o.MaxLatency)
		}
		uri += srtEncryptionParams(o.Encryption)
		return uri
	case ProtocolSRTCaller:
		uri := "srt://" + o.Destination + ":" + strconv.Itoa(o.Port) + "?mode=caller"
		if o.MaxLatency > 0 {
			uri += "&latency=" + strconv.Itoa(o.MaxLatency)
		}
		uri += srtEncryptionParams(o.Encryption)
		return uri
	case ProtocolRTP, ProtocolRTPFEC:
		return "rtp://" + o.Destination + ":" + strconv.Itoa(o.Port)
	case ProtocolRIST:
		return "rist://" + o.Destination + ":" + strconv.Itoa(o.Port)
	case ProtocolUDP:
		return "udp://" + o.Destination + ":" + strconv.Itoa(o.Port)
	case ProtocolRTMP:
		key := o.StreamKey
		if key == "" {
			key = "stream"
		}
		return "rtmp://" + o.Destination + ":" + strconv.Itoa(o.Port) + "/live/" + key
	default:
		return ""
	}
}

// srtEncryptionParams returns SRT URI params for passphrase encryption
func srtEncryptionParams(enc *Encryption) string {
	if enc == nil || enc.Passphrase == "" {
		return ""
	}
	params := "&passphrase=" + url.QueryEscape(enc.Passphrase)
	switch enc.Algorithm {
	case "aes128":
		params += "&pbkeylen=16"
	case "aes192":
		params += "&pbkeylen=24"
	case "aes256":
		params += "&pbkeylen=32"
	default:
		// FFmpeg default is AES-128
	}
	return params
}

// DeepCopy returns a deep copy of the Flow to avoid data races
func (f *Flow) DeepCopy() *Flow {
	cp := *f
	if f.Source != nil {
		s := *f.Source
		if f.Source.Decryption != nil {
			d := *f.Source.Decryption
			s.Decryption = &d
		}
		cp.Source = &s
	}
	if len(f.Sources) > 0 {
		cp.Sources = make([]*Source, len(f.Sources))
		for i, s := range f.Sources {
			sCopy := *s
			if s.Decryption != nil {
				d := *s.Decryption
				sCopy.Decryption = &d
			}
			cp.Sources[i] = &sCopy
		}
	}
	if len(f.Outputs) > 0 {
		cp.Outputs = make([]*Output, len(f.Outputs))
		for i, o := range f.Outputs {
			oCopy := *o
			if len(o.CidrAllowList) > 0 {
				oCopy.CidrAllowList = make([]string, len(o.CidrAllowList))
				copy(oCopy.CidrAllowList, o.CidrAllowList)
			}
			if o.Encryption != nil {
				e := *o.Encryption
				oCopy.Encryption = &e
			}
			cp.Outputs[i] = &oCopy
		}
	}
	if f.SourceFailoverConfig != nil {
		fc := *f.SourceFailoverConfig
		if f.SourceFailoverConfig.SourcePriority != nil {
			sp := *f.SourceFailoverConfig.SourcePriority
			fc.SourcePriority = &sp
		}
		cp.SourceFailoverConfig = &fc
	}
	if f.SourceMonitorConfig != nil {
		mc := *f.SourceMonitorConfig
		cp.SourceMonitorConfig = &mc
	}
	if f.MaintenanceWindow != nil {
		mw := *f.MaintenanceWindow
		cp.MaintenanceWindow = &mw
	}
	if f.StartedAt != nil {
		t := *f.StartedAt
		cp.StartedAt = &t
	}
	if f.StoppedAt != nil {
		t := *f.StoppedAt
		cp.StoppedAt = &t
	}
	return &cp
}

func listenAddr(port int) string {
	return "0.0.0.0:" + strconv.Itoa(port)
}
