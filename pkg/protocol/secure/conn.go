// ABOUTME: The encrypted WebSocket connection — drives the init/noise preamble
// ABOUTME: from either side and frames all subsequent traffic through the Session.
package secure

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// DefaultHandshakeTimeout bounds each expected message during the preamble,
// per the spec's failure-handling recommendation.
const DefaultHandshakeTimeout = 30 * time.Second

// handshakeReadLimit caps preamble frame sizes; init and noise messages are
// all well under this.
const handshakeReadLimit = 64 << 10

// Conn is an established encrypted Sendspin connection. All traffic after the
// preamble travels as Noise-encrypted binary WebSocket frames; JSON control
// messages are transport type 0. Reads must come from a single goroutine;
// writes are internally serialized.
type Conn struct {
	ws      *websocket.Conn
	sess    *Session
	reasm   *Reassembler
	writeMu sync.Mutex
	peerID  string
	// ctx drives transport reads/writes (coder/websocket requires a context).
	ctx context.Context

	// Retained for in-band re-handshakes: client_id/server_id and suite
	// carry over, and the new prologue is the prior handshake's hash.
	identity *Identity
	peerPub  []byte
	suite    Suite
}

// ServerHandshakeConfig configures the server side of the preamble. The
// server is the Noise initiator regardless of which side dialed.
type ServerHandshakeConfig struct {
	Identity *Identity
	// SelectPSK returns the PSK to mix for this client and its category,
	// which is announced in noise message 1 — pairing-record lookup in a
	// real server. Nil selects the Sentinel PSK (category 'sn') for every
	// client (unpaired-access only).
	SelectPSK func(clientID string) (PSK, PSKCategoryCode, error)
	// HandshakeTimeout bounds each expected preamble message; 0 selects
	// DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// MaxMessageSize bounds reassembled message sizes post-handshake; 0
	// selects DefaultMaxMessageSize.
	MaxMessageSize int
}

// ServerHandshake performs the server side of the connection preamble:
// read client/init, send server/init, send Noise message 1, read Noise
// message 2, switch to transport mode. On any failure the socket is closed
// without an application-level error, per spec.
func ServerHandshake(ws *websocket.Conn, cfg ServerHandshakeConfig) (conn *Conn, ci ClientInit, err error) {
	defer func() {
		if err != nil {
			ws.Close(websocket.StatusNormalClosure, "")
		}
	}()
	if cfg.Identity == nil {
		return nil, ClientInit{}, fmt.Errorf("server handshake requires an identity")
	}
	timeout := cfg.HandshakeTimeout
	if timeout == 0 {
		timeout = DefaultHandshakeTimeout
	}
	ws.SetReadLimit(handshakeReadLimit)

	clientInitRaw, err := readTextFrame(ws, timeout)
	if err != nil {
		return nil, ClientInit{}, fmt.Errorf("read client/init: %w", err)
	}
	ci, err = ParseClientInit(clientInitRaw)
	if err != nil {
		// Init failure: send server/error with the classified reason, then
		// close (per the spec's failure handling). Unauthenticated, so the
		// reason is a hint. The deferred close runs on the way out.
		reason := classifyInitFailure(clientInitRaw)
		if serr, merr := EncodeServerError(reason); merr == nil {
			_ = writeTextFrame(ws, serr, timeout)
		}
		return nil, ClientInit{}, fmt.Errorf("client/init rejected (%s): %w", reason, err)
	}
	clientPub, err := ParseID(ci.ClientID)
	if err != nil {
		return nil, ClientInit{}, err
	}

	serverInitRaw, err := EncodeServerInit(ServerInit{ServerID: cfg.Identity.ID(), Version: CoreVersion})
	if err != nil {
		return nil, ClientInit{}, err
	}
	if err = writeTextFrame(ws, serverInitRaw, timeout); err != nil {
		return nil, ClientInit{}, fmt.Errorf("write server/init: %w", err)
	}

	psk := SentinelPSK()
	category := CategorySentinel
	if cfg.SelectPSK != nil {
		psk, category, err = cfg.SelectPSK(ci.ClientID)
		if err != nil {
			return nil, ClientInit{}, fmt.Errorf("select psk: %w", err)
		}
		if !category.Valid() {
			return nil, ClientInit{}, fmt.Errorf("select psk: invalid category %q", string(category))
		}
	}

	init, err := NewInitiator(InitiatorConfig{
		Identity:      cfg.Identity,
		PeerPublicKey: clientPub,
		Suite:         ci.Suite,
		Prologue:      Prologue(clientInitRaw, serverInitRaw),
		PSK:           psk,
	})
	if err != nil {
		return nil, ClientInit{}, err
	}
	msg1, err := init.Message1(psk.ID(), category)
	if err != nil {
		return nil, ClientInit{}, err
	}
	msg1Env, err := EncodeNoiseHandshake(msg1)
	if err != nil {
		return nil, ClientInit{}, err
	}
	if err = writeTextFrame(ws, msg1Env, timeout); err != nil {
		return nil, ClientInit{}, fmt.Errorf("write noise message 1: %w", err)
	}

	msg2Env, err := readTextFrame(ws, timeout)
	if err != nil {
		return nil, ClientInit{}, fmt.Errorf("read noise message 2: %w", err)
	}
	msg2, err := ParseNoiseHandshake(msg2Env)
	if err != nil {
		return nil, ClientInit{}, err
	}
	sess, err := init.Finish(msg2)
	if err != nil {
		return nil, ClientInit{}, err
	}

	c := newConn(ws, sess, ci.ClientID, cfg.MaxMessageSize)
	c.identity, c.peerPub, c.suite = cfg.Identity, clientPub, ci.Suite
	return c, ci, nil
}

// ClientHandshakeConfig configures the client side of the preamble.
type ClientHandshakeConfig struct {
	Identity *Identity
	// ExpectedServerID pins the server identity (stored-pubkey model). Empty
	// accepts the server/init identity at face value (shared-PSK model).
	ExpectedServerID string
	// Suite is the cipher suite to announce; empty selects SuiteChaChaPoly.
	Suite Suite
	// SelectPSK maps the psk_id + psk_category from Noise message 1 to a
	// candidate PSK (a psk_id held under a different category is a miss).
	// Nil accepts only the Sentinel PSK when the announced category is 'sn'.
	SelectPSK func(pskID string, category PSKCategoryCode) (PSK, bool)
	// SentinelFallback enables the initial-handshake Sentinel fallback on a
	// lookup miss (default true; re-handshakes pass false).
	SentinelFallback *bool
	// HandshakeTimeout bounds each expected preamble message; 0 selects
	// DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// MaxMessageSize bounds reassembled message sizes post-handshake; 0
	// selects DefaultMaxMessageSize.
	MaxMessageSize int
}

// ClientHandshake performs the client side of the connection preamble:
// send client/init, read server/init, respond to the Noise handshake, switch
// to transport mode. On any failure the socket is closed without an
// application-level error, per spec.
func ClientHandshake(ws *websocket.Conn, cfg ClientHandshakeConfig) (conn *Conn, si ServerInit, err error) {
	defer func() {
		if err != nil {
			ws.Close(websocket.StatusNormalClosure, "")
		}
	}()
	if cfg.Identity == nil {
		return nil, ServerInit{}, fmt.Errorf("client handshake requires an identity")
	}
	suite := cfg.Suite
	if suite == "" {
		suite = SuiteChaChaPoly
	}
	selectPSK := cfg.SelectPSK
	if selectPSK == nil {
		selectPSK = func(pskID string, category PSKCategoryCode) (PSK, bool) {
			if category != CategorySentinel {
				return PSK{}, false
			}
			s := SentinelPSK()
			if pskID == s.ID() {
				return s, true
			}
			return PSK{}, false
		}
	}
	fallback := true
	if cfg.SentinelFallback != nil {
		fallback = *cfg.SentinelFallback
	}
	timeout := cfg.HandshakeTimeout
	if timeout == 0 {
		timeout = DefaultHandshakeTimeout
	}
	ws.SetReadLimit(handshakeReadLimit)

	clientInitRaw, err := EncodeClientInit(ClientInit{ClientID: cfg.Identity.ID(), Version: CoreVersion, Suite: suite})
	if err != nil {
		return nil, ServerInit{}, err
	}
	if err = writeTextFrame(ws, clientInitRaw, timeout); err != nil {
		return nil, ServerInit{}, fmt.Errorf("write client/init: %w", err)
	}

	serverInitRaw, err := readTextFrame(ws, timeout)
	if err != nil {
		return nil, ServerInit{}, fmt.Errorf("read server/init: %w", err)
	}
	si, err = ParseServerInit(serverInitRaw)
	if err != nil {
		return nil, ServerInit{}, err
	}
	if cfg.ExpectedServerID != "" && si.ServerID != cfg.ExpectedServerID {
		return nil, ServerInit{}, fmt.Errorf("server identity %q does not match expected %q", si.ServerID, cfg.ExpectedServerID)
	}
	serverPub, err := ParseID(si.ServerID)
	if err != nil {
		return nil, ServerInit{}, err
	}

	msg1Env, err := readTextFrame(ws, timeout)
	if err != nil {
		return nil, ServerInit{}, fmt.Errorf("read noise message 1: %w", err)
	}
	msg1, err := ParseNoiseHandshake(msg1Env)
	if err != nil {
		return nil, ServerInit{}, err
	}

	resp, err := NewResponder(ResponderConfig{
		Identity:         cfg.Identity,
		PeerPublicKey:    serverPub,
		Suite:            suite,
		Prologue:         Prologue(clientInitRaw, serverInitRaw),
		SelectPSK:        selectPSK,
		SentinelFallback: fallback,
	})
	if err != nil {
		return nil, ServerInit{}, err
	}
	msg2, sess, err := resp.Accept(msg1)
	if err != nil {
		return nil, ServerInit{}, err
	}
	msg2Env, err := EncodeNoiseHandshake(msg2)
	if err != nil {
		return nil, ServerInit{}, err
	}
	if err = writeTextFrame(ws, msg2Env, timeout); err != nil {
		return nil, ServerInit{}, fmt.Errorf("write noise message 2: %w", err)
	}

	c := newConn(ws, sess, si.ServerID, cfg.MaxMessageSize)
	c.identity, c.peerPub, c.suite = cfg.Identity, serverPub, suite
	return c, si, nil
}

func newConn(ws *websocket.Conn, sess *Session, peerID string, maxMessageSize int) *Conn {
	// Transport frames are ciphertexts of at most maxNoiseMessage bytes.
	ws.SetReadLimit(maxNoiseMessage + 1024)
	// coder/websocket derives per-message timeouts from the context passed to
	// Read/Write — there are no SetReadDeadline/SetWriteDeadline methods, and a
	// context that fires closes the connection. The handshake helper uses a
	// deadline-bound context per frame (readTextFrame/writeTextFrame); transport
	// mode must carry none, or the handshake timeout would leak into every
	// streaming write and silently kill long-lived connections. c.ctx is
	// deadline-free, so transport reads/writes are never timed out; liveness is
	// handled by the read side.
	return &Conn{
		ws:     ws,
		sess:   sess,
		reasm:  NewReassembler(maxMessageSize),
		peerID: peerID,
		ctx:    context.Background(),
	}
}

// PeerID returns the peer's wire identity: the client_id on a server-side
// connection, the server_id on a client-side one.
func (c *Conn) PeerID() string { return c.peerID }

// Suite returns the negotiated cipher suite, needed for PSK wrapping.
func (c *Conn) Suite() Suite { return c.suite }

// HandshakeHash returns the Noise handshake hash h of this connection.
func (c *Conn) HandshakeHash() []byte { return c.sess.HandshakeHash() }

// WriteMessage encrypts and sends one message, fragmenting as needed.
func (c *Conn) WriteMessage(msgType byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	for _, frame := range Fragment(msgType, payload) {
		ct, err := c.sess.Encrypt(frame)
		if err != nil {
			return err
		}
		if err := c.ws.Write(c.ctx, websocket.MessageBinary, ct); err != nil {
			return fmt.Errorf("write transport frame: %w", err)
		}
	}
	return nil
}

// WriteJSON marshals v and sends it as a JSON control message (type 0).
func (c *Conn) WriteJSON(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal control message: %w", err)
	}
	return c.WriteMessage(MsgTypeJSON, body)
}

// ReadMessage receives the next complete message, decrypting and reassembling
// fragments. A text frame after transport mode, an AEAD failure, or a framing
// violation is terminal: the error is returned and the caller must close.
func (c *Conn) ReadMessage() (msgType byte, payload []byte, err error) {
	for {
		wsType, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return 0, nil, err
		}
		if wsType != websocket.MessageBinary {
			return 0, nil, fmt.Errorf("non-binary WebSocket frame in transport mode")
		}
		pt, err := c.sess.Decrypt(data)
		if err != nil {
			return 0, nil, err
		}
		msgType, payload, done, err := c.reasm.Feed(pt)
		if err != nil {
			return 0, nil, err
		}
		if done {
			return msgType, payload, nil
		}
	}
}

// Close closes the underlying WebSocket.
func (c *Conn) Close() error { return c.ws.Close(websocket.StatusNormalClosure, "") }

// CloseNow closes the underlying WebSocket immediately, without attempting a
// close handshake. Prefer this over Close when the peer has been told we are
// leaving (e.g. a client/goodbye has just been written): a graceful Close
// waits up to 5s for the peer to echo the close frame, which strands a
// reader when the peer has already stopped reading.
func (c *Conn) CloseNow() error { return c.ws.CloseNow() }

func readTextFrame(ws *websocket.Conn, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	msgType, data, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if msgType != websocket.MessageText {
		return nil, fmt.Errorf("expected text frame during handshake, got type %d", msgType)
	}
	return data, nil
}

func writeTextFrame(ws *websocket.Conn, data []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return ws.Write(ctx, websocket.MessageText, data)
}
