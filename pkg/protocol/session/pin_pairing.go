// ABOUTME: PIN pairing flow choreography — dynamic and static, both sides,
// ABOUTME: over the CPace primitives and re-handshake in pkg secure. Wire
// ABOUTME: shapes follow the current spec (derivation, sid round, sealed
// ABOUTME: nonce_B/PSK).
package session

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"

	"github.com/Sendspin/sendspin-go/pkg/protocol/secure"
)

// b64 is unpadded base64url, the encoding for every 32/48/64-byte pairing
// value carried on the wire.
var b64 = base64.RawURLEncoding

// Pair-abort reasons produced by the pairing flows (spec vocabulary).
const (
	AbortPairingCodeMismatch string = "pairing_code_mismatch"
	AbortAttemptTimeout      string = "attempt_timeout"
	AbortConcurrentAttempt   string = "concurrent_attempt"
	AbortMethodNotSupported  string = "method_not_supported"
	AbortUserCancelled       string = "user_cancelled"

	// AbortPINMismatch is the pre-spec spelling, kept as an alias.
	AbortPINMismatch = AbortPairingCodeMismatch
)

// PINPairingServerConfig configures the server side of a PIN pairing attempt.
type PINPairingServerConfig struct {
	// Method is dynamic_pairing_code or static_pairing_code.
	Method string
	// PairingIndex is the number of pairing server/activate messages since
	// the last Noise handshake — the value EstablishServer used, fed into the
	// CPace sid and checked against the client's pairing_index.
	PairingIndex uint32
	// Format is the dynamic emission format (digits or qr_code). Ignored for
	// static PIN.
	Format string
	// ProvideCode returns the pairing code the operator entered, as its PRS
	// bytes: the 6 ASCII digits for dynamic digits, the 24 raw bytes for
	// dynamic qr_code, or the 8 ASCII digits for static.
	ProvideCode func() ([]byte, error)
	// Persist stores the unwrapped long-term PSK for this client.
	Persist func(clientID string, psk secure.PSK) error
	// Resulting is the post-pairing session configuration (typically
	// playback at long-term trust). PSKCategory is forced to PSKLongTerm.
	Resulting ServerConfig
}

// CompletePINPairing runs the server side of a PIN pairing attempt on a
// session whose initial activate declared ['pairing'] with a PIN method. On
// success it persists the record, re-handshakes to the new PSK, and
// re-establishes the session. A pairing failure sends pair/abort and returns
// a *PairAbortError; the connection stays open for a retry.
func (s *ServerSession) CompletePINPairing(cfg PINPairingServerConfig) (*ServerSession, error) {
	h := s.Conn.HandshakeHash()
	round := uint32(1) // single round in this flow; retries are a follow-up
	sid := secure.BuildPakeSID(h, cfg.PairingIndex, round)

	// 1. client/pair-init: attempt start + (dynamic) commit_B.
	msg, err := readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read client/pair-init: %w", err)
	}
	init, ok := msg.(*ClientPairInit)
	if !ok {
		if ab, isAbort := msg.(*PairAbort); isAbort {
			return nil, &PairAbortError{Reason: ab.Reason}
		}
		return nil, fmt.Errorf("expected client/pair-init, got %T", msg)
	}
	if uint32(init.PairingIndex) > cfg.PairingIndex {
		// A higher pairing_index is a protocol error; a lower one is a
		// superseded attempt — both end the synchronous entry point.
		return nil, fmt.Errorf("pairing_index %d out of range (counter %d)", init.PairingIndex, cfg.PairingIndex)
	}
	dynamic := cfg.Method == string(PairMethodDynamicPairingCode)
	var commitB []byte
	if dynamic {
		if init.CommitB == "" {
			return nil, fmt.Errorf("dynamic code: client/pair-init missing commit_B")
		}
		commitB, err = b64.DecodeString(init.CommitB)
		if err != nil {
			return nil, fmt.Errorf("decode commit_B: %w", err)
		}
	}

	// 2. (dynamic) server/pair-init: nonce_A only.
	var nonceA []byte
	if dynamic {
		nonceA = make([]byte, 32)
		if _, err := rand.Read(nonceA); err != nil {
			return nil, fmt.Errorf("sample nonce_A: %w", err)
		}
		if err := writeMessage(s.Conn, ServerPairInit{NonceA: b64.EncodeToString(nonceA)}); err != nil {
			return nil, fmt.Errorf("send server/pair-init: %w", err)
		}
	}

	// The operator now enters the code (dynamic: read from the client's
	// out-channel; static: typed off the device).
	prs, err := cfg.ProvideCode()
	if err != nil {
		_ = writeMessage(s.Conn, PairAbort{Reason: AbortUserCancelled})
		return nil, fmt.Errorf("obtain operator code: %w", err)
	}

	// 3. CPace as initiator (role A).
	cp, err := secure.StartCPace(secure.CPaceInitiator, prs, sid, nil, secure.CPaceADServer)
	if err != nil {
		return nil, fmt.Errorf("start cpace: %w", err)
	}
	if err := writeMessage(s.Conn, ServerPairAuth{PakeMsg1: b64.EncodeToString(cp.PublicShare())}); err != nil {
		return nil, fmt.Errorf("send server/pair-auth: %w", err)
	}

	// 4. client/pair-auth: peer share.
	msg, err = readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read client/pair-auth: %w", err)
	}
	auth, ok := msg.(*ClientPairAuth)
	if !ok {
		return nil, unexpectedOrAbort(msg, "client/pair-auth")
	}
	peerShare, err := b64.DecodeString(auth.PakeMsg2)
	if err != nil {
		return nil, fmt.Errorf("decode pake_msg_2: %w", err)
	}
	if err := cp.Derive(peerShare, secure.CPaceADClient); err != nil {
		return nil, fmt.Errorf("cpace derive: %w", err)
	}

	// 5. server/pair-confirm: our MCF tag.
	tag, err := cp.Tag()
	if err != nil {
		return nil, err
	}
	if err := writeMessage(s.Conn, ServerPairConfirm{ServerKC: b64.EncodeToString(tag)}); err != nil {
		return nil, fmt.Errorf("send server/pair-confirm: %w", err)
	}

	// 6. client/pair-confirm: peer tag + (dynamic) wrapped_nonce_B.
	msg, err = readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read client/pair-confirm: %w", err)
	}
	confirm, ok := msg.(*ClientPairConfirm)
	if !ok {
		return nil, unexpectedOrAbort(msg, "client/pair-confirm")
	}
	peerTag, err := b64.DecodeString(confirm.ClientKC)
	if err != nil {
		return nil, fmt.Errorf("decode client_kc: %w", err)
	}

	var nonceB []byte
	if dynamic {
		wrapped, err := b64.DecodeString(confirm.WrappedNonceB)
		if err != nil {
			return nil, fmt.Errorf("decode wrapped_nonce_B: %w", err)
		}
		isk, err := cp.ISK()
		if err != nil {
			return nil, err
		}
		nonceB, err = secure.UnwrapNonceB(sid, isk, wrapped, s.Conn.Suite())
		if err != nil {
			// A wrapped_nonce_B that fails to decrypt is a protocol error.
			s.Conn.Close()
			return nil, fmt.Errorf("unwrap nonce_B (protocol error): %w", err)
		}
		if subtle.ConstantTimeCompare(secure.CommitB(nonceB), commitB) != 1 {
			// A nonce that doesn't open the commitment is a protocol error.
			s.Conn.Close()
			return nil, fmt.Errorf("nonce_B does not open commit_B (protocol error)")
		}
	}
	if !cp.Verify(peerTag) {
		return s.abortCode(AbortPairingCodeMismatch)
	}

	// 7. client/pair-finalize: wrapped PSK.
	msg, err = readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read client/pair-finalize: %w", err)
	}
	fin, ok := msg.(*ClientPairFinalize)
	if !ok {
		return nil, unexpectedOrAbort(msg, "client/pair-finalize")
	}
	if fin.WrappedPSK == "" || fin.LongTermPSK != "" {
		return nil, fmt.Errorf("pin flow requires wrapped_psk (and only it)")
	}
	wrapped, err := b64.DecodeString(fin.WrappedPSK)
	if err != nil {
		return nil, fmt.Errorf("decode wrapped_psk: %w", err)
	}
	psk, err := secure.UnwrapPSK(sid, mustISK(cp), wrapped, s.Conn.Suite())
	if err != nil {
		// A wrapped_psk that fails to decrypt is a protocol error.
		s.Conn.Close()
		return nil, fmt.Errorf("unwrap psk (protocol error): %w", err)
	}
	// The delivered PSK must not equal the Sentinel or this connection's
	// pairing PSK (protocol error, persist nothing).
	if psk == secure.SentinelPSK() {
		s.Conn.Close()
		return nil, fmt.Errorf("delivered long-term PSK equals the Sentinel PSK (protocol error)")
	}
	if err := cfg.Persist(s.Conn.PeerID(), psk); err != nil {
		return nil, fmt.Errorf("persist pairing record: %w", err)
	}

	// 8. server/pair-finalize, then re-handshake to the new PSK and
	// re-establish the session at long-term trust.
	if err := writeMessage(s.Conn, ServerPairFinalize{}); err != nil {
		return nil, fmt.Errorf("send server/pair-finalize: %w", err)
	}
	if err := s.Conn.RehandshakeInitiate(psk); err != nil {
		return nil, fmt.Errorf("re-handshake to paired psk: %w", err)
	}
	cfg.Resulting.PSKCategory = PSKLongTerm
	return EstablishServer(s.Conn, cfg.Resulting)
}

func mustISK(cp *secure.CPace) []byte {
	isk, _ := cp.ISK()
	return isk
}

func (s *ServerSession) abortCode(reason string) (*ServerSession, error) {
	_ = writeMessage(s.Conn, PairAbort{Reason: reason})
	return nil, &PairAbortError{Reason: reason}
}

func unexpectedOrAbort(msg any, want string) error {
	if ab, ok := msg.(*PairAbort); ok {
		return &PairAbortError{Reason: ab.Reason}
	}
	return fmt.Errorf("expected %s, got %T", want, msg)
}

// PINPairingClientConfig configures the client side of a PIN pairing attempt.
type PINPairingClientConfig struct {
	// Method is dynamic_pairing_code or static_pairing_code.
	Method string
	// PairingIndex mirrors the server's index (from the activate that opened
	// this attempt); used for the pairing_index and CPace sid.
	PairingIndex uint32
	// Format is the dynamic emission format (digits or qr_code).
	Format string
	// StaticCode is the device's printed code (static method only).
	StaticCode string
	// EmitCode receives the pairing code to present to the operator: the
	// 6-digit code for dynamic digits, the SP: v1 token for dynamic qr_code,
	// or the static 8-digit code.
	EmitCode func(code string)
	// NewPSK mints the long-term PSK to deliver; nil generates a random one.
	NewPSK func() (secure.PSK, error)
	// Persist stores the delivered PSK locally against the server_id.
	Persist func(serverID string, psk secure.PSK) error
	// ResultingHello is the client/hello for the re-established session.
	ResultingHello ClientHello
}

// RunPINPairing runs the client side of a PIN pairing attempt on a session
// whose activate selected a PIN method. On success it persists the record,
// responds to the server's re-handshake, and re-establishes the session. A
// local check failure sends pair/abort and returns a *PairAbortError.
func (s *ClientSession) RunPINPairing(cfg PINPairingClientConfig) (*ClientSession, error) {
	h := s.Conn.HandshakeHash()
	round := uint32(1)
	sid := secure.BuildPakeSID(h, cfg.PairingIndex, round)

	dynamic := cfg.Method == string(PairMethodDynamicPairingCode)

	// 1. client/pair-init.
	var nonceB []byte
	init := ClientPairInit{PairingIndex: int(cfg.PairingIndex)}
	if dynamic {
		nonceB = make([]byte, 32)
		if _, err := rand.Read(nonceB); err != nil {
			return nil, fmt.Errorf("sample nonce_B: %w", err)
		}
		init.CommitB = b64.EncodeToString(secure.CommitB(nonceB))
	}
	if err := writeMessage(s.Conn, init); err != nil {
		return nil, fmt.Errorf("send client/pair-init: %w", err)
	}

	code := cfg.StaticCode

	// 2. (dynamic) server/pair-init: nonce_A → derive & emit the code.
	var nonceA []byte
	if dynamic {
		msg, err := readControl(s.Conn)
		if err != nil {
			return nil, fmt.Errorf("read server/pair-init: %w", err)
		}
		spi, ok := msg.(*ServerPairInit)
		if !ok {
			return nil, unexpectedOrAbort(msg, "server/pair-init")
		}
		nonceA, err = b64.DecodeString(spi.NonceA)
		if err != nil {
			return nil, fmt.Errorf("decode nonce_A: %w", err)
		}
		switch cfg.Format {
		case string(PairingFormatDigits):
			code = secure.DerivePairingDigits(h, nonceA, nonceB)
		default: // qr_code
			qr := secure.DerivePairingQR(h, nonceA, nonceB)
			tok, err := secure.PairingTokenV1(qr)
			if err != nil {
				return nil, fmt.Errorf("encode qr pairing token: %w", err)
			}
			code = tok
		}
		if cfg.EmitCode != nil {
			cfg.EmitCode(code)
		}
	}

	// 3. server/pair-auth → CPace as responder (role B); PRS for the static
	// flow is the printed 8-digit code; for dynamic digits the 6 ASCII
	// digits; for dynamic qr_code the 24-byte version-1 token payload.
	msg, err := readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read server/pair-auth: %w", err)
	}
	spa, ok := msg.(*ServerPairAuth)
	if !ok {
		return nil, unexpectedOrAbort(msg, "server/pair-auth")
	}
	serverShare, err := b64.DecodeString(spa.PakeMsg1)
	if err != nil {
		return nil, fmt.Errorf("decode pake_msg_1: %w", err)
	}
	prs, err := s.codePRS(cfg, dynamic, code)
	if err != nil {
		return nil, err
	}
	cp, err := secure.StartCPace(secure.CPaceResponder, prs, sid, nil, secure.CPaceADClient)
	if err != nil {
		return nil, fmt.Errorf("start cpace: %w", err)
	}
	if err := cp.Derive(serverShare, secure.CPaceADServer); err != nil {
		return nil, fmt.Errorf("cpace derive: %w", err)
	}
	if err := writeMessage(s.Conn, ClientPairAuth{PakeMsg2: b64.EncodeToString(cp.PublicShare())}); err != nil {
		return nil, fmt.Errorf("send client/pair-auth: %w", err)
	}

	// 4. server/pair-confirm → verify server tag.
	msg, err = readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read server/pair-confirm: %w", err)
	}
	spc, ok := msg.(*ServerPairConfirm)
	if !ok {
		return nil, unexpectedOrAbort(msg, "server/pair-confirm")
	}
	serverTag, err := b64.DecodeString(spc.ServerKC)
	if err != nil {
		return nil, fmt.Errorf("decode server_kc: %w", err)
	}
	if !cp.Verify(serverTag) {
		return s.abortCodeClient(AbortPairingCodeMismatch)
	}

	// 5. client/pair-confirm + client/pair-finalize (back to back).
	confirm := ClientPairConfirm{ClientKC: b64.EncodeToString(mustTag(cp))}
	if dynamic {
		isk, err := cp.ISK()
		if err != nil {
			return nil, err
		}
		wrapped, err := secure.WrapNonceB(sid, isk, nonceB, s.Conn.Suite())
		if err != nil {
			return nil, fmt.Errorf("wrap nonce_B: %w", err)
		}
		confirm.WrappedNonceB = b64.EncodeToString(wrapped)
	}
	if err := writeMessage(s.Conn, confirm); err != nil {
		return nil, fmt.Errorf("send client/pair-confirm: %w", err)
	}

	newPSK := secure.SentinelPSK() // placeholder; replaced below
	if cfg.NewPSK != nil {
		newPSK, err = cfg.NewPSK()
	} else {
		newPSK, err = secure.NewPSK()
	}
	if err != nil {
		return nil, fmt.Errorf("mint long-term psk: %w", err)
	}
	wrapped, err := secure.WrapPSK(sid, mustISK(cp), newPSK, s.Conn.Suite())
	if err != nil {
		return nil, fmt.Errorf("wrap psk: %w", err)
	}
	if err := writeMessage(s.Conn, ClientPairFinalize{WrappedPSK: b64.EncodeToString(wrapped)}); err != nil {
		return nil, fmt.Errorf("send client/pair-finalize: %w", err)
	}

	// 6. server/pair-finalize → persist, respond to re-handshake, re-establish.
	msg, err = readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read server/pair-finalize: %w", err)
	}
	if ab, isAbort := msg.(*PairAbort); isAbort {
		return nil, &PairAbortError{Reason: ab.Reason}
	}
	if _, ok := msg.(*ServerPairFinalize); !ok {
		return nil, fmt.Errorf("expected server/pair-finalize, got %T", msg)
	}
	if err := cfg.Persist(s.Conn.PeerID(), newPSK); err != nil {
		return nil, fmt.Errorf("persist pairing record: %w", err)
	}

	mt, payload, err := s.Conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read rehandshake message 1: %w", err)
	}
	if mt != secure.MsgTypeJSON || !isNoiseHandshake(payload) {
		return nil, fmt.Errorf("expected noise/handshake after pairing, got type %d", mt)
	}
	if err := s.Conn.RehandshakeRespond(payload, func(pskID string) (secure.PSK, bool) {
		if pskID == newPSK.ID() {
			return newPSK, true
		}
		return secure.PSK{}, false
	}); err != nil {
		return nil, fmt.Errorf("re-handshake to paired psk: %w", err)
	}
	return EstablishClient(s.Conn, ClientConfig{Hello: cfg.ResultingHello, PSKCategory: PSKLongTerm})
}

// codePRS converts the operator-facing code into the CPace PRS byte string:
// ASCII digits for digits/static, or the 24-byte payload of a version-1 token
// for dynamic qr_code.
func (s *ClientSession) codePRS(cfg PINPairingClientConfig, dynamic bool, code string) ([]byte, error) {
	if dynamic && cfg.Format == string(PairingFormatQR) {
		_, payload, err := secure.DecodePairingToken(code)
		if err != nil {
			return nil, fmt.Errorf("qr pairing token: %w", err)
		}
		return payload, nil
	}
	return []byte(code), nil
}

func (s *ClientSession) abortCodeClient(reason string) (*ClientSession, error) {
	_ = writeMessage(s.Conn, PairAbort{Reason: reason})
	return nil, &PairAbortError{Reason: reason}
}

func mustTag(cp *secure.CPace) []byte {
	tag, _ := cp.Tag()
	return tag
}
