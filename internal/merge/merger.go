package merge

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// RTPMerger implements SMPTE ST 2022-7 hitless merging of two redundant RTP streams.
// It receives RTP packets from two sources, deduplicates by sequence number,
// fills gaps from the other source, and outputs a clean merged stream.
type RTPMerger struct {
	portA, portB int    // ingest ports for source A and B
	outputPort   int    // local port where merged stream is written
	recoveryMs   int    // recovery window in milliseconds
	connA, connB *net.UDPConn
	outputConn   *net.UDPConn
	outputAddr   *net.UDPAddr

	buffer   map[uint16]*rtpPacket // indexed by RTP sequence number
	bufMu    sync.Mutex
	nextSeq  uint16 // next expected sequence number to output
	started  bool
	seqInit  bool // whether nextSeq has been initialized

	statsA        mergeSourceStats
	statsB        mergeSourceStats
	outputCount   int64
	missingCount  int64
	statsMu       sync.RWMutex

	stopCh chan struct{}
	done   chan struct{}
}

type rtpPacket struct {
	seq       uint16
	data      []byte
	arrivedAt time.Time
	fromA     bool // which source it came from
}

type mergeSourceStats struct {
	PacketsReceived int64
	PacketsUnique   int64 // non-duplicate packets contributed to merged output
	PacketsDuped    int64 // duplicate packets (already had from other source)
}

// MergeStats returns merge statistics
type MergeStats struct {
	SourceAPackets int64 `json:"source_a_packets"`
	SourceAUsed    int64 `json:"source_a_gap_fills"`
	SourceADuped   int64 `json:"source_a_duplicates"`
	SourceBPackets int64 `json:"source_b_packets"`
	SourceBUsed    int64 `json:"source_b_gap_fills"`
	SourceBDuped   int64 `json:"source_b_duplicates"`
	OutputPackets  int64 `json:"output_packets"`
	MissingPackets int64 `json:"missing_packets"` // gaps not filled by either source
}

// NewRTPMerger creates a merger for two RTP source ports.
// outputPort is the local UDP port where the clean merged stream will be available.
func NewRTPMerger(portA, portB, outputPort, recoveryMs int) *RTPMerger {
	if recoveryMs <= 0 {
		recoveryMs = 100 // default 100ms recovery window
	}
	return &RTPMerger{
		portA:      portA,
		portB:      portB,
		outputPort: outputPort,
		recoveryMs: recoveryMs,
		buffer:     make(map[uint16]*rtpPacket),
		stopCh:     make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// OutputPort returns the local port where the merged stream is available
func (m *RTPMerger) OutputPort() int {
	return m.outputPort
}

// Start begins the merge operation
func (m *RTPMerger) Start() error {
	// Bind listeners for both sources
	var err error
	m.connA, err = net.ListenUDP("udp", &net.UDPAddr{Port: m.portA})
	if err != nil {
		return fmt.Errorf("listen source A on port %d: %w", m.portA, err)
	}
	m.connB, err = net.ListenUDP("udp", &net.UDPAddr{Port: m.portB})
	if err != nil {
		m.connA.Close()
		return fmt.Errorf("listen source B on port %d: %w", m.portB, err)
	}

	// Create output connection (send to localhost)
	m.outputAddr = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: m.outputPort}
	m.outputConn, err = net.DialUDP("udp", nil, m.outputAddr)
	if err != nil {
		m.connA.Close()
		m.connB.Close()
		return fmt.Errorf("dial output port %d: %w", m.outputPort, err)
	}

	// Set read buffer sizes
	m.connA.SetReadBuffer(2 * 1024 * 1024) // 2MB
	m.connB.SetReadBuffer(2 * 1024 * 1024)

	m.started = true

	// Start receiver goroutines
	go m.receiveLoop(m.connA, true)
	go m.receiveLoop(m.connB, false)

	// Start output goroutine
	go m.outputLoop()

	log.Printf("[merge] Started: portA=%d portB=%d -> output=%d (recovery=%dms)",
		m.portA, m.portB, m.outputPort, m.recoveryMs)
	return nil
}

// Stop halts the merger
func (m *RTPMerger) Stop() {
	if !m.started {
		return
	}
	close(m.stopCh)
	m.connA.Close()
	m.connB.Close()
	m.outputConn.Close()
	<-m.done
}

// Stats returns current merge statistics
func (m *RTPMerger) Stats() MergeStats {
	m.statsMu.RLock()
	defer m.statsMu.RUnlock()
	return MergeStats{
		SourceAPackets: m.statsA.PacketsReceived,
		SourceAUsed:    m.statsA.PacketsUnique,
		SourceADuped:   m.statsA.PacketsDuped,
		SourceBPackets: m.statsB.PacketsReceived,
		SourceBUsed:    m.statsB.PacketsUnique,
		SourceBDuped:   m.statsB.PacketsDuped,
		OutputPackets:  m.outputCount,
		MissingPackets: m.missingCount,
	}
}

// receiveLoop reads RTP packets from one source and buffers them
func (m *RTPMerger) receiveLoop(conn *net.UDPConn, isA bool) {
	buf := make([]byte, 1500) // standard MTU
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := conn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-m.stopCh:
				return
			default:
				continue
			}
		}

		if n < 12 {
			continue // too small for RTP header
		}

		// Parse RTP sequence number (bytes 2-3, big-endian)
		seq := uint16(buf[2])<<8 | uint16(buf[3])

		// Copy packet data
		data := make([]byte, n)
		copy(data, buf[:n])

		pkt := &rtpPacket{
			seq:       seq,
			data:      data,
			arrivedAt: time.Now(),
			fromA:     isA,
		}

		m.bufMu.Lock()

		// Update stats
		m.statsMu.Lock()
		if isA {
			m.statsA.PacketsReceived++
		} else {
			m.statsB.PacketsReceived++
		}

		// Check if we already have this sequence number (duplicate)
		if existing, ok := m.buffer[seq]; ok {
			if isA {
				m.statsA.PacketsDuped++
			} else {
				m.statsB.PacketsDuped++
			}
			_ = existing // keep the first arrival
			m.statsMu.Unlock()
			m.bufMu.Unlock()
			continue
		}

		// New packet - insert into buffer
		m.buffer[seq] = pkt

		// Track unique contributions (non-duplicate packets that expand the buffer)
		if isA {
			m.statsA.PacketsUnique++
		} else {
			m.statsB.PacketsUnique++
		}
		m.statsMu.Unlock()

		// Initialize sequence tracking on first packet
		if !m.seqInit {
			m.nextSeq = seq
			m.seqInit = true
		}

		m.bufMu.Unlock()
	}
}

// outputLoop reads from the merge buffer in sequence order and writes to output
func (m *RTPMerger) outputLoop() {
	defer close(m.done)

	recoveryDuration := time.Duration(m.recoveryMs) * time.Millisecond
	ticker := time.NewTicker(200 * time.Microsecond) // 5000 packets/sec check rate
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
		}

		if !m.seqInit {
			continue
		}

		m.bufMu.Lock()

		// Try to output packets in sequence order
		for {
			pkt, ok := m.buffer[m.nextSeq]
			if ok {
				// Got the next packet - send it
				m.outputConn.Write(pkt.data)
				delete(m.buffer, m.nextSeq)
				m.nextSeq++
				m.statsMu.Lock()
				m.outputCount++
				m.statsMu.Unlock()
				continue
			}

			// Packet missing - check if we should wait or skip
			// Look at the oldest buffered packet to see how far ahead we are
			oldestWait := time.Duration(0)
			for _, p := range m.buffer {
				// Check sequence distance (with wrapping)
				dist := seqDistance(m.nextSeq, p.seq)
				if dist > 0 && dist < 1000 {
					// There are future packets buffered, meaning this gap has had time
					elapsed := time.Since(p.arrivedAt)
					if elapsed > oldestWait {
						oldestWait = elapsed
					}
				}
			}

			if oldestWait > recoveryDuration {
				// Recovery window exceeded - skip this packet
				m.statsMu.Lock()
				m.missingCount++
				m.statsMu.Unlock()
				m.nextSeq++
				continue
			}

			// Still within recovery window, wait for it
			break
		}

		// Prune very old buffer entries (more than 5x recovery window behind)
		for seq := range m.buffer {
			if seqDistance(seq, m.nextSeq) > 5000 {
				delete(m.buffer, seq)
			}
		}

		m.bufMu.Unlock()
	}
}

// seqDistance returns the forward distance from a to b in sequence space (uint16 wrapping)
func seqDistance(a, b uint16) int {
	diff := int(b) - int(a)
	if diff < 0 {
		diff += 65536
	}
	return diff
}
