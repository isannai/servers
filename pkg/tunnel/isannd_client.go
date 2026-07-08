package tunnel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Frame types on the control-plane TCP stream. Duplicated from
// pkg/signal/proto.go to avoid an import cycle (pkg/signal imports
// pkg/tunnel for RendezvousMsg, so pkg/tunnel cannot import back).
// Keep these values in sync with pkg/signal/proto.go.
const (
	frameTypeHello         = "hello"
	frameTypeRegister      = "register"
	frameTypeAck           = "ack"
	frameTypeConnect       = "connect"
	frameTypeProxyInfo     = "proxy_info"
	frameTypeError         = "error"
	frameTypeNeedRegister  = "need_register"
	frameTypePing          = "ping"
	frameTypeServiceEvent  = "service_event"
)

// frameMaxSize bounds an inbound frame body to 1 MiB. Same cap as
// pkg/signal — registers can be a few hundred KB at worst (hardware
// inventory + services), so 1 MiB has plenty of headroom while still
// catching corruption / DoS attempts that send a multi-GB length.
const frameMaxSize = 1 << 20

// writeFrameTo writes a length-prefixed JSON frame: [4 byte big-endian
// length][JSON body of msg]. Caller serialises writes to the same conn.
func writeFrameTo(w io.Writer, msg *RendezvousMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("frame marshal: %w", err)
	}
	if len(data) > frameMaxSize {
		return errors.New("frame too large")
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

// readFrameFrom reads one length-prefixed JSON frame, decodes into a
// fresh RendezvousMsg. Returns io.EOF cleanly on closed conn.
func readFrameFrom(r io.Reader) (*RendezvousMsg, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, errors.New("frame: zero length")
	}
	if n > frameMaxSize {
		return nil, fmt.Errorf("frame too large (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("frame read body: %w", err)
	}
	var msg RendezvousMsg
	if err := json.Unmarshal(buf, &msg); err != nil {
		return nil, fmt.Errorf("frame unmarshal: %w", err)
	}
	return &msg, nil
}

// IsanndClient is the bridge provider / broker containers use to hand
// RV-bound messages to the host's isannd. Two transports:
//
//   - TCP NLB socket (long-lived) — register / heartbeat / service_event
//     frames go out via length-prefixed JSON write (writeFrameTo); server-push
//     (ack / proxy_info / need_register) comes back via a reader goroutine.
//     The bytes pass unmodified through isannd's rv-control listener to RV's
//     :9100 TCP control endpoint (with isannd intercepting `punch` frames
//     locally so they fire from isannd's NAT-mapped UDP socket).
//
//   - HTTP client (request-response) — GetTPMFingerprint and any future
//     read endpoint that doesn't need streaming semantics.
//
// Construction is cheap. Connect() establishes the TCP socket and sends
// the hello frame; callers must invoke it before the first Send* call.
// All Send* methods are safe to call from multiple goroutines (Write is
// serialised by writeMu).
type IsanndClient struct {
	httpBase    string
	controlAddr string
	hc          *http.Client

	// hello identity — captured by Connect, replayed on every reconnect.
	helloID   string
	helloRole string

	// TCP state — protected by stateMu when we swap conn / writer on
	// reconnect. WriteFrame still uses writeMu so the active conn is
	// not mid-write when we replace it.
	stateMu  sync.Mutex
	conn     net.Conn
	writeMu  sync.Mutex
	dialing  atomic.Bool
	closed   atomic.Bool

	// Server-push state — reader goroutine sets these.
	needRegisterFlag atomic.Bool

	// pushCallbacks — optional listeners for ack / proxy_info / etc.
	// frames the reader sees. Keyed by frame Type; nil callbacks skip
	// dispatch. Set via OnPush; not protected by mutex because it's
	// expected to be configured once before Connect.
	pushCallbacks map[string]func(*RendezvousMsg)
}

// NewIsanndClient creates a client pointed at isannd's node-bridge HTTP
// URL and rv-control TCP host:port. Empty values fall back to localhost
// defaults (HTTP `127.0.0.1:8443`, TCP `127.0.0.1:19100`).
func NewIsanndClient(httpBase, controlAddr string) *IsanndClient {
	if httpBase == "" {
		httpBase = "http://127.0.0.1:8443"
	}
	if controlAddr == "" {
		controlAddr = "127.0.0.1:19100"
	}
	return &IsanndClient{
		httpBase:    httpBase,
		controlAddr: controlAddr,
		hc:          &http.Client{Timeout: 10 * time.Second},
	}
}

// OnPush registers a callback for a frame Type seen on the server-push
// stream (incoming RV → backend frames). Useful for ack/proxy_info logs
// or future broker-side connect coordination. Must be called before
// Connect — not safe to mutate concurrently with the reader.
func (c *IsanndClient) OnPush(frameType string, cb func(*RendezvousMsg)) {
	if c.pushCallbacks == nil {
		c.pushCallbacks = make(map[string]func(*RendezvousMsg))
	}
	c.pushCallbacks[frameType] = cb
}

// Connect establishes the TCP NLB socket and sends the hello frame.
// Identity (nodeID + role) is captured so the client can re-send hello
// on every reconnect without the caller's help. Reader goroutine starts
// here and lives until Close.
//
// Idempotent: calling Connect twice with the same identity is a no-op.
// Calling with a different identity is a programming error.
func (c *IsanndClient) Connect(ctx context.Context, nodeID, role string) error {
	if c.closed.Load() {
		return errors.New("isannd client closed")
	}
	c.stateMu.Lock()
	if c.helloID != "" && c.helloID != nodeID {
		c.stateMu.Unlock()
		return fmt.Errorf("isannd client already connected as %s (cannot switch to %s)", c.helloID, nodeID)
	}
	c.helloID = nodeID
	c.helloRole = role
	already := c.conn != nil
	c.stateMu.Unlock()
	if already {
		return nil
	}
	return c.dialAndHello(ctx)
}

// dialAndHello opens the TCP socket, writes the hello frame, and spawns
// the reader goroutine. Called by Connect and by the reconnect path.
// dialing flag prevents two concurrent dials from racing.
func (c *IsanndClient) dialAndHello(ctx context.Context) error {
	if !c.dialing.CompareAndSwap(false, true) {
		return nil // another goroutine is already reconnecting
	}
	defer c.dialing.Store(false)

	c.stateMu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	nodeID := c.helloID
	role := c.helloRole
	c.stateMu.Unlock()
	if nodeID == "" {
		return errors.New("isannd client: hello identity not set")
	}

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.controlAddr)
	if err != nil {
		return fmt.Errorf("isannd tcp dial %s: %w", c.controlAddr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	hello := &RendezvousMsg{V: 1, Type: frameTypeHello, ID: nodeID, Role: role}
	if err := writeFrameTo(conn, hello); err != nil {
		_ = conn.Close()
		return fmt.Errorf("isannd write hello: %w", err)
	}

	c.stateMu.Lock()
	c.conn = conn
	c.stateMu.Unlock()

	go c.runReader(conn, nodeID)
	log.Printf("[isannd-client] connected %s as %s (role=%s)", c.controlAddr, nodeID, role)
	return nil
}

// runReader pumps frames from the TCP socket. The byte pipe and isannd
// have stripped out `punch` already; what reaches here is server-push
// for the backend to consume (ack / proxy_info / need_register).
//
// On error the reader exits and the connection is marked dead. The next
// Send* call triggers a fresh dialAndHello.
func (c *IsanndClient) runReader(conn net.Conn, nodeID string) {
	for {
		msg, err := readFrameFrom(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !c.closed.Load() {
				log.Printf("[isannd-client] reader (%s): %v", nodeID, err)
			}
			c.markConnDead(conn)
			return
		}
		switch msg.Type {
		case frameTypeNeedRegister:
			c.needRegisterFlag.Store(true)
			log.Printf("[isannd-client] need_register received for %s", msg.ID)
		case frameTypeAck, frameTypeError, frameTypeProxyInfo:
			// dispatch to caller callback if registered; otherwise log.
			if cb, ok := c.pushCallbacks[msg.Type]; ok && cb != nil {
				cb(msg)
			}
		default:
			if cb, ok := c.pushCallbacks[msg.Type]; ok && cb != nil {
				cb(msg)
			}
		}
	}
}

// markConnDead clears c.conn if it still matches the reader's snapshot.
// Avoids racing with a fresh dialAndHello that may have already replaced
// the conn pointer.
func (c *IsanndClient) markConnDead(dead net.Conn) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.conn == dead {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// ensureConn returns the active TCP connection, dialing if needed.
// Callers must use this under writeMu so the conn doesn't flip mid-send.
func (c *IsanndClient) ensureConn(ctx context.Context) (net.Conn, error) {
	c.stateMu.Lock()
	conn := c.conn
	c.stateMu.Unlock()
	if conn != nil {
		return conn, nil
	}
	if err := c.dialAndHello(ctx); err != nil {
		return nil, err
	}
	c.stateMu.Lock()
	conn = c.conn
	c.stateMu.Unlock()
	if conn == nil {
		return nil, errors.New("isannd client: dial succeeded but conn nil")
	}
	return conn, nil
}

// writeFrame serialises a frame onto the TCP socket. On write failure
// the conn is marked dead so the next call reconnects.
//
// Lazy identity capture: if Connect() wasn't called explicitly, the
// first frame's ID + Role seed the hello identity for reconnects.
// Subsequent frames with a different ID are rejected to avoid leaking
// one node's frames onto another's control socket.
func (c *IsanndClient) writeFrame(ctx context.Context, msg *RendezvousMsg) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.stateMu.Lock()
	if c.helloID == "" && msg.ID != "" {
		c.helloID = msg.ID
		c.helloRole = msg.Role
	} else if msg.ID != "" && c.helloID != msg.ID {
		c.stateMu.Unlock()
		return fmt.Errorf("isannd client identity mismatch: hello=%s msg=%s", c.helloID, msg.ID)
	}
	c.stateMu.Unlock()
	conn, err := c.ensureConn(ctx)
	if err != nil {
		return err
	}
	if err := writeFrameTo(conn, msg); err != nil {
		c.markConnDead(conn)
		return fmt.Errorf("isannd write %s: %w", msg.Type, err)
	}
	return nil
}

// SendRegister sends a register frame. The caller's signature must
// already be attached. isannd forwards the bytes verbatim to RV.
func (c *IsanndClient) SendRegister(ctx context.Context, msg *RendezvousMsg) error {
	if msg == nil {
		return errors.New("nil register msg")
	}
	if msg.Type == "" {
		msg.Type = frameTypeRegister
	}
	return c.writeFrame(ctx, msg)
}

// SendHeartbeat sends a ping frame and returns (needRegister, err).
// needRegister=true means the reader observed a `need_register` frame
// since the last call; the flag is cleared on read. err covers send
// failure only — server-push values arrive asynchronously, so a
// successful Write doesn't guarantee RV processed the ping.
func (c *IsanndClient) SendHeartbeat(ctx context.Context, ping *HeartbeatPing) (bool, error) {
	if ping == nil {
		return false, errors.New("nil heartbeat ping")
	}
	frame := &RendezvousMsg{
		V:           1,
		Type:        frameTypePing,
		ID:          ping.NodeID,
		Role:        ping.Role,
		TimestampMs: ping.TimestampMs,
		Signature:   ping.Signature,
	}
	if err := c.writeFrame(ctx, frame); err != nil {
		return false, err
	}
	return c.needRegisterFlag.Swap(false), nil
}

// SendMetrics sends a service_event frame carrying one service's metric
// snapshot. Event-driven push only — not on a timer.
//
// Prefer SendMetricsBatch for multi-service updates; this method exists
// for single-service callers and back-compat with older provider builds.
func (c *IsanndClient) SendMetrics(ctx context.Context, evt *MetricsEvent) error {
	if evt == nil {
		return errors.New("nil metrics event")
	}
	frame := &RendezvousMsg{
		V:       1,
		Type:    frameTypeServiceEvent,
		ID:      evt.NodeID,
		Role:    evt.Role,
		Metrics: &evt.Service,
	}
	return c.writeFrame(ctx, frame)
}

// SendMetricsBatch sends one service_event frame carrying multiple
// services' metric snapshots. Provider's pushAllMetrics coalesces a burst
// of queue lifecycle callbacks and calls this once per debounce window so
// a single push reflects every enabled service's current state.
//
// nodeID / role pin the frame to the sending node; batch is the per-
// service rows. An empty batch is still sent (as `metrics_batch:[]`) so
// the receiver gets a positive "I checked, nothing has value" signal
// instead of a silent no-send.
func (c *IsanndClient) SendMetricsBatch(ctx context.Context, nodeID, role string, batch []ServiceMetrics) error {
	if batch == nil {
		batch = []ServiceMetrics{}
	}
	frame := &RendezvousMsg{
		V:            1,
		Type:         frameTypeServiceEvent,
		ID:           nodeID,
		Role:         role,
		MetricsBatch: batch,
	}
	return c.writeFrame(ctx, frame)
}

// SendFrame writes any RendezvousMsg verbatim. Used for shapes that don't
// fit Register/Heartbeat/Metrics — most notably service.starting /
// service.ready / service.stopped lifecycle events with Inspect payload,
// which need to ride the same TCP NLB conn that handles register.
//
// Callers MUST set msg.Type. msg.ID is filled in here when empty so RV
// can route on identity. The frame is forwarded to RV byte-verbatim by
// isannd's nlb_listener pipe.
func (c *IsanndClient) SendFrame(ctx context.Context, msg *RendezvousMsg) error {
	if msg == nil {
		return errors.New("nil frame")
	}
	if msg.Type == "" {
		return errors.New("frame missing type")
	}
	return c.writeFrame(ctx, msg)
}

// Close shuts the TCP socket and stops the reader. Idempotent.
func (c *IsanndClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.stateMu.Lock()
	conn := c.conn
	c.conn = nil
	c.stateMu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// GetTPMFingerprint asks isannd for the host's TPM EK fingerprint via
// HTTP — kept on HTTP because it's a one-shot read with no streaming
// semantics, and TPM access doesn't benefit from the long-lived control
// socket. Returns ("", nil) when isannd reports TPM unavailable —
// caller decides whether to proceed without it.
func (c *IsanndClient) GetTPMFingerprint(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.httpBase+"/internal/api/tpm/measurement", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("isannd /tpm/measurement: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Fingerprint, nil
}
