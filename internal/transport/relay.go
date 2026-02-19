package transport

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// PacketRelay receives UDP/mpegts packets from an input FFmpeg process
// and copies each packet to all registered output ports. This enables
// hitless output add/remove: the input FFmpeg and relay run continuously
// while individual output FFmpeg processes can be started/stopped independently.
type PacketRelay struct {
	inputConn *net.UDPConn
	inputPort int
	outputs   map[string]*relayOutput
	mu        sync.RWMutex
	stopCh    chan struct{}
	stopped   bool
	done      chan struct{}
}

type relayOutput struct {
	name string
	port int
	conn *net.UDPConn
}

// NewPacketRelay creates a relay that binds to an OS-assigned local port.
func NewPacketRelay() *PacketRelay {
	return &PacketRelay{
		outputs: make(map[string]*relayOutput),
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start binds to a local port and begins relaying packets.
func (r *PacketRelay) Start() error {
	var err error
	r.inputConn, err = net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 0, // OS-assigned
	})
	if err != nil {
		return err
	}
	r.inputConn.SetReadBuffer(4 * 1024 * 1024) // 4MB receive buffer
	r.inputPort = r.inputConn.LocalAddr().(*net.UDPAddr).Port

	go r.run()
	return nil
}

// InputPort returns the port where the input FFmpeg should send packets.
func (r *PacketRelay) InputPort() int {
	return r.inputPort
}

// Stop halts the relay and closes all connections.
func (r *PacketRelay) Stop() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	// Close all output connections under the lock to prevent AddOutput race
	for _, out := range r.outputs {
		out.conn.Close()
	}
	r.outputs = make(map[string]*relayOutput)
	r.mu.Unlock()

	close(r.stopCh)
	r.inputConn.Close()
	<-r.done
}

// AddOutput registers a new output destination. The relay will begin
// copying packets to the given local port immediately. The output FFmpeg
// should already be listening on this port.
func (r *PacketRelay) AddOutput(name string, localPort int) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("relay is stopped")
	}
	// Close existing connection for this name to avoid leaking
	if old, exists := r.outputs[name]; exists {
		old.conn.Close()
	}
	r.mu.Unlock()

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: localPort}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return err
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		conn.Close()
		return fmt.Errorf("relay is stopped")
	}
	r.outputs[name] = &relayOutput{name: name, port: localPort, conn: conn}
	r.mu.Unlock()

	log.Printf("[relay] Added output %s -> 127.0.0.1:%d", name, localPort)
	return nil
}

// RemoveOutput unregisters an output. Packets will no longer be copied to it.
func (r *PacketRelay) RemoveOutput(name string) {
	r.mu.Lock()
	out, ok := r.outputs[name]
	if ok {
		out.conn.Close()
		delete(r.outputs, name)
	}
	r.mu.Unlock()

	if ok {
		log.Printf("[relay] Removed output %s", name)
	}
}

// HasOutput checks if an output is registered.
func (r *PacketRelay) HasOutput(name string) bool {
	r.mu.RLock()
	_, ok := r.outputs[name]
	r.mu.RUnlock()
	return ok
}

// run is the main relay loop: read a packet, copy to all outputs.
func (r *PacketRelay) run() {
	defer close(r.done)

	buf := make([]byte, 65536)
	for {
		r.inputConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := r.inputConn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				select {
				case <-r.stopCh:
					return
				default:
					continue
				}
			}
			select {
			case <-r.stopCh:
				return
			default:
				continue
			}
		}

		// Copy packet to all registered outputs
		r.mu.RLock()
		for _, out := range r.outputs {
			_, _ = out.conn.Write(buf[:n])
		}
		r.mu.RUnlock()
	}
}
