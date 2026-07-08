package tunnel

import (
	"net"
	"sync"
)

// UDPPacket holds a UDP packet with its source address.
type UDPPacket struct {
	Data []byte
	Addr net.Addr
}

// Packet prefix bytes (first byte of UDP payload).
//
//	0x00 → rendezvous signaling JSON (SigCh)
//	0x01 → ICE-lite hole-punch ping (PunchCh) — used for prflx discovery
//	other → QUIC datagram (quicCh)
//
// PrefixHeart (0x02) was the legacy UDP heartbeat path; removed alongside
// RV's UDP+HTTP/3 listener. Metrics now flow over the TCP NLB to RV.
const (
	PrefixSignal = 0x00
	PrefixPunch  = 0x01
)

// MuxPacketConn splits incoming UDP packets by prefix:
//
//	0x00 → SigCh (rendezvous signaling)
//	0x01 → PunchCh (ICE-lite hole-punch)
//	else → quicCh (QUIC datagram)
type MuxPacketConn struct {
	net.PacketConn
	SigCh     chan UDPPacket
	PunchCh   chan UDPPacket
	quicCh    chan UDPPacket
	done      chan struct{}
	closeOnce sync.Once
}

// NewMuxPacketConn creates a new muxed packet connection.
func NewMuxPacketConn(pc net.PacketConn) *MuxPacketConn {
	m := &MuxPacketConn{
		PacketConn: pc,
		SigCh:      make(chan UDPPacket, 64),
		PunchCh:    make(chan UDPPacket, 64),
		quicCh:     make(chan UDPPacket, 512),
		done:       make(chan struct{}),
	}
	go m.dispatch()
	return m
}

func (m *MuxPacketConn) dispatch() {
	defer close(m.done)
	buf := make([]byte, 65536)
	for {
		n, addr, err := m.PacketConn.ReadFrom(buf)
		if err != nil {
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		if n > 0 && data[0] == PrefixSignal {
			select {
			case m.SigCh <- UDPPacket{Data: data[1:], Addr: addr}:
			default:
			}
		} else if n > 0 && data[0] == PrefixPunch {
			select {
			case m.PunchCh <- UDPPacket{Data: data[1:], Addr: addr}:
			default:
			}
		} else {
			select {
			case m.quicCh <- UDPPacket{Data: data, Addr: addr}:
			default:
			}
		}
	}
}

// ReadFrom returns only QUIC packets (called by quic-go).
func (m *MuxPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case pkt := <-m.quicCh:
		n := copy(b, pkt.Data)
		return n, pkt.Addr, nil
	case <-m.done:
		return 0, nil, net.ErrClosed
	}
}

// Close closes the underlying connection.
func (m *MuxPacketConn) Close() error {
	var err error
	m.closeOnce.Do(func() {
		err = m.PacketConn.Close()
	})
	return err
}
