// Package signal implements QUIC-based reliable signaling for rendezvous.
//
// Wire protocol:
//   - ALPN: "isann-signal"
//   - Stream framing: [4-byte big-endian length][JSON body]
//   - Body: reuses tunnel.RendezvousMsg + new "hello" type
//
// Streams per connection:
//   - Control bidi stream (first stream opened by client): hello, register,
//     server-push events (punch).
//   - Request streams (opened ad-hoc per RPC): connect → proxy_info.
package signal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/isannai/isann-servers/pkg/tunnel"
)

// ALPN identifier for the signaling QUIC connection.
const ALPN = "isann-signal"

// Message types carried in the signaling stream.
// Reuses tunnel.RendezvousMsg; these string constants document valid values.
const (
	TypeHello      = "hello"      // client → server, first frame on control stream
	TypeRegister   = "register"   // client → server, periodic
	TypeAck        = "ack"        // server → client, reply to register/hello
	TypeConnect    = "connect"    // client (broker) → server, RPC on new stream
	TypeProxyInfo  = "proxy_info" // server → client, reply to connect
	TypePunch      = "punch"      // server → provider, push on control stream
	TypeError      = "error"      // server → client
	TypeStatusUpd  = "status_update"
	TypeServiceEvent = "service_event" // provider → server, pushed on service state transition
	TypePing       = "ping"         // app-layer keepalive (either direction)
	TypeNeedRegister = "need_register" // server → client, pushed when ping arrives without entry (RV restart recovery)
)

// Max frame size (guard against malicious / bogus length prefix).
// 1 MiB is far above any legitimate signaling payload.
const maxFrameSize = 1 << 20

// ErrFrameTooLarge is returned when a received frame exceeds maxFrameSize.
var ErrFrameTooLarge = errors.New("signal: frame exceeds max size")

// WriteFrame serializes msg as JSON and writes [4-byte BE length][json] to w.
func WriteFrame(w io.Writer, msg *tunnel.RendezvousMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("signal: marshal: %w", err)
	}
	if len(data) > maxFrameSize {
		return ErrFrameTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one [4-byte BE length][json] frame from r.
func ReadFrame(r io.Reader) (*tunnel.RendezvousMsg, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, errors.New("signal: zero-length frame")
	}
	if n > maxFrameSize {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("signal: read body: %w", err)
	}
	var msg tunnel.RendezvousMsg
	if err := json.Unmarshal(buf, &msg); err != nil {
		return nil, fmt.Errorf("signal: unmarshal: %w", err)
	}
	return &msg, nil
}
