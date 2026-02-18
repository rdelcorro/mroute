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

// IsValidProtocol checks if a protocol is supported
func IsValidProtocol(p Protocol) bool {
	return ValidProtocols[p]
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
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	Status               FlowStatus      `json:"status"`
	Source               *Source         `json:"source"`
	Sources              []*Source       `json:"sources,omitempty"`
	Outputs              []*Output       `json:"outputs"`
	SourceFailoverConfig *FailoverConfig `json:"source_failover_config,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
	StartedAt            *time.Time      `json:"started_at,omitempty"`
	StoppedAt            *time.Time      `json:"stopped_at,omitempty"`
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
	Status           OutputStatus `json:"status"`
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
	FlowID        string           `json:"flow_id"`
	ActiveSource  string           `json:"active_source"`
	Status        FlowStatus       `json:"status"`
	UptimeSeconds int64            `json:"uptime_seconds"`
	SourceMetrics []*SourceMetrics `json:"source_metrics"`
	OutputMetrics []*OutputMetrics `json:"output_metrics"`
	FailoverCount int64            `json:"failover_count"`
	Timestamp     time.Time        `json:"timestamp"`
}

// SourceMetrics holds metrics for a single source
type SourceMetrics struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	Connected       bool   `json:"connected"`
	BitrateKbps     int64  `json:"bitrate_kbps"`
	PacketsReceived int64  `json:"packets_received"`
	PacketsLost     int64  `json:"packets_lost"`
	RoundTripTimeMs int64  `json:"round_trip_time_ms"`
}

// OutputMetrics holds metrics for a single output
type OutputMetrics struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Connected   bool   `json:"connected"`
	BitrateKbps int64  `json:"bitrate_kbps"`
	PacketsSent int64  `json:"packets_sent"`
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
	// Validate StreamID (used in RTMP/SRT URIs - must not contain whitespace or URI-breaking chars)
	if s.StreamID != "" && strings.ContainsAny(s.StreamID, " \t\n\r?&#") {
		return fmt.Errorf("source %s: stream_id must not contain whitespace or special URI characters", s.Name)
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
	// Non-listener outputs need a valid destination (IP or hostname)
	if o.Protocol != ProtocolSRTListener {
		if o.Destination == "" {
			return fmt.Errorf("output %s: destination required for protocol %s", o.Name, o.Protocol)
		}
		if err := validateHost(o.Destination); err != nil {
			return fmt.Errorf("output %s: invalid destination: %w", o.Name, err)
		}
	}
	// Validate StreamKey (used in RTMP URIs - must not contain whitespace or URI-breaking chars)
	if o.StreamKey != "" && strings.ContainsAny(o.StreamKey, " \t\n\r?&#/") {
		return fmt.Errorf("output %s: stream_key must not contain whitespace or special URI characters", o.Name)
	}
	return nil
}

// validateHost checks that a string is a valid IP address or hostname
func validateHost(h string) error {
	if net.ParseIP(h) != nil {
		return nil
	}
	// Check for URI-breaking characters (basic hostname validation)
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
			uri += "&latency=" + strconv.Itoa(s.MaxLatency) // FFmpeg SRT expects ms
		}
		if s.MaxBitrate > 0 {
			uri += "&maxbw=" + strconv.Itoa(s.MaxBitrate) // bytes/sec
		}
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
		return "rtmp://0.0.0.0:" + strconv.Itoa(s.IngestPort) + "/live/" + streamKey + " live=1"
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
		return uri
	case ProtocolSRTCaller:
		uri := "srt://" + o.Destination + ":" + strconv.Itoa(o.Port) + "?mode=caller"
		if o.MaxLatency > 0 {
			uri += "&latency=" + strconv.Itoa(o.MaxLatency)
		}
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

// DeepCopy returns a deep copy of the Flow to avoid data races
func (f *Flow) DeepCopy() *Flow {
	cp := *f
	if f.Source != nil {
		s := *f.Source
		cp.Source = &s
	}
	if len(f.Sources) > 0 {
		cp.Sources = make([]*Source, len(f.Sources))
		for i, s := range f.Sources {
			sCopy := *s
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
