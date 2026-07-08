package signal

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/isannai/isann-servers/pkg/tunnel"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  *tunnel.RendezvousMsg
	}{
		{
			name: "minimal",
			msg:  &tunnel.RendezvousMsg{V: 1, Type: TypeAck},
		},
		{
			name: "hello",
			msg:  &tunnel.RendezvousMsg{V: 1, Type: TypeHello, Role: "provider", ID: "P:0xABC", CertHash: "deadbeef"},
		},
		{
			name: "connect",
			msg:  &tunnel.RendezvousMsg{V: 1, Type: TypeConnect, ID: "P:target"},
		},
		{
			name: "proxy_info with candidates",
			msg: &tunnel.RendezvousMsg{
				V:          1,
				Type:       TypeProxyInfo,
				Addr:       "1.2.3.4:4433",
				Candidates: []string{"1.2.3.4:4433", "10.0.0.1:4433"},
				CertHash:   "aa",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, c.msg); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Type != c.msg.Type || got.ID != c.msg.ID || got.Addr != c.msg.Addr {
				t.Fatalf("mismatch: got=%+v want=%+v", got, c.msg)
			}
			if len(got.Candidates) != len(c.msg.Candidates) {
				t.Fatalf("candidates mismatch: got=%v want=%v", got.Candidates, c.msg.Candidates)
			}
		})
	}
}

func TestFrameMultiple(t *testing.T) {
	// Two frames back-to-back must decode independently.
	var buf bytes.Buffer
	a := &tunnel.RendezvousMsg{V: 1, Type: TypeHello, ID: "A"}
	b := &tunnel.RendezvousMsg{V: 1, Type: TypeRegister, ID: "B"}
	if err := WriteFrame(&buf, a); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, b); err != nil {
		t.Fatal(err)
	}
	gotA, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if gotA.ID != "A" || gotB.ID != "B" {
		t.Fatalf("order wrong: %q %q", gotA.ID, gotB.ID)
	}
}

func TestFrameEOF(t *testing.T) {
	// Empty reader must return io.EOF on header read.
	_, err := ReadFrame(strings.NewReader(""))
	if err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

func TestFrameZeroLength(t *testing.T) {
	// Zero length header must be rejected.
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(0))
	_, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("want error on zero-length frame")
	}
}

func TestFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	// Write header claiming 2 MiB (> 1 MiB cap).
	binary.Write(&buf, binary.BigEndian, uint32(2<<20))
	_, err := ReadFrame(&buf)
	if err != ErrFrameTooLarge {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestFrameTruncated(t *testing.T) {
	// Header says 100 bytes, only 10 available.
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(100))
	buf.Write(make([]byte, 10))
	_, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("want error on truncated body")
	}
}
