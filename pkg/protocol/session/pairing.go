// ABOUTME: Pairing — record store, the Pairing-PSK flow on both sides, and
// ABOUTME: the post-pairing re-handshake + session re-establishment.
package session

import (
	"encoding/json"
	"fmt"

	"github.com/0xk1f0/spinsync/pkg/protocol/secure"
)

// Pair method identifiers (deprecated spellings retained so the pairing
// internals below stay usable; the wire identifiers live in messages.go and
// match the spec: pairing_psk / dynamic_pairing_code / static_pairing_code).
const (
	PairMethodDynamicPIN string = "dynamic_pairing_code"
	PairMethodStaticPIN  string = "static_pairing_code"
)

// Pairing message payloads (registered in messages.go's envelope registry).

// ClientPairPending reports that the client is holding back the selected
// attempt (no pairing window open, or the round limit holds it back). It does
// not start the attempt or its timeout.
type ClientPairPending struct {
	PairingIndex int     `json:"pairing_index"`
	Message      *string `json:"message,omitempty"`
}

// ClientPairInit starts a pairing attempt: attempt start plus, for the
// dynamic pairing code flow, the commitment commit_B.
type ClientPairInit struct {
	PairingIndex int    `json:"pairing_index"`
	CommitB      string `json:"commit_B,omitempty"`
}

// ServerPairInit carries the server nonce in dynamic-code pairing.
type ServerPairInit struct {
	NonceA string `json:"nonce_A"`
}

// ServerPairAuth carries the server's CPace public share.
type ServerPairAuth struct {
	PakeMsg1 string `json:"pake_msg_1"`
}

// ClientPairAuth carries the client's CPace public share.
type ClientPairAuth struct {
	PakeMsg2 string `json:"pake_msg_2"`
}

// ServerPairConfirm carries the server's MCF tag.
type ServerPairConfirm struct {
	ServerKC string `json:"server_kc"`
}

// ClientPairRetry asks the server for a new pairing round when the client's
// verification of server_kc failed. Sent in place of client/pair-confirm.
type ClientPairRetry struct{}

// ClientPairConfirm carries the client's MCF tag and, in dynamic-code
// pairing, the sealed commit opening wrapped_nonce_B.
type ClientPairConfirm struct {
	ClientKC      string `json:"client_kc"`
	WrappedNonceB string `json:"wrapped_nonce_B,omitempty"`
}

// ClientPairFinalize delivers the long-term PSK — directly in the Pairing
// PSK flow, wrapped under the CPace output in the PIN flows. Exactly one
// field is present.
type ClientPairFinalize struct {
	LongTermPSK string `json:"long_term_psk,omitempty"`
	WrappedPSK  string `json:"wrapped_psk,omitempty"`
}

// ServerPairFinalize acknowledges the persisted pairing record.
type ServerPairFinalize struct{}

// ServerUnpair is sent by a paired server to drop its own pairing record from
// the client: the client removes the record, sends client/goodbye reason
// 'unpaired', and closes; an unpaired session ignores it.
type ServerUnpair struct{}

// PairAbort aborts a pairing attempt.
type PairAbort struct {
	Reason string `json:"reason"`
}

// PairAbortError reports a pairing flow ended by the peer's pair/abort.
type PairAbortError struct {
	Reason string
}

func (e *PairAbortError) Error() string { return "pairing aborted: " + e.Reason }

// PairingRecord is one stored (peer, PSK) association.
type PairingRecord struct {
	PeerID   string // client_id on servers, server_id on clients; "" for shared-PSK records
	PSK      secure.PSK
	Category PSKCategory
}

// MemoryPairingStore is an in-memory pairing store implementing the spec's
// server-side resolve order: long-term record → staged Pairing PSK →
// Sentinel. Not safe for concurrent use without external locking.
type MemoryPairingStore struct {
	records map[string]secure.PSK // peerID → long-term PSK
	staged  map[string]secure.PSK // peerID → staged Pairing PSK
}

// NewMemoryPairingStore creates an empty store.
func NewMemoryPairingStore() *MemoryPairingStore {
	return &MemoryPairingStore{
		records: map[string]secure.PSK{},
		staged:  map[string]secure.PSK{},
	}
}

// StagePairingPSK stages a Pairing PSK for a specific client ahead of a
// pairing attempt.
func (m *MemoryPairingStore) StagePairingPSK(peerID string, psk secure.PSK) {
	m.staged[peerID] = psk
}

// PutRecord persists a long-term record for a peer, clearing any staged
// Pairing PSK.
func (m *MemoryPairingStore) PutRecord(peerID string, psk secure.PSK) error {
	m.records[peerID] = psk
	delete(m.staged, peerID)
	return nil
}

// Records returns the stored long-term records.
func (m *MemoryPairingStore) Records() []PairingRecord {
	out := []PairingRecord{}
	for id, psk := range m.records {
		out = append(out, PairingRecord{PeerID: id, PSK: psk, Category: PSKLongTerm})
	}
	return out
}

// Resolve returns the PSK and category to use for a peer, per the spec's
// order: long-term record, then staged Pairing PSK, then Sentinel.
func (m *MemoryPairingStore) Resolve(peerID string) (secure.PSK, PSKCategory) {
	if psk, ok := m.records[peerID]; ok {
		return psk, PSKLongTerm
	}
	if psk, ok := m.staged[peerID]; ok {
		return psk, PSKPairing
	}
	return secure.SentinelPSK(), PSKSentinel
}

// CompletePairingPSK runs the server side of the Pairing PSK flow on a
// session whose initial activate declared ['pairing'] with method
// pairing_psk: the client MUST send client/pair-init followed immediately by
// client/pair-finalize; the server consumes both, persists the delivered PSK,
// acknowledges, re-handshakes to the new PSK, and re-establishes the session
// under `resulting` (typically playback at long-term trust). Returns the new
// session; the old one is dead.
func (s *ServerSession) CompletePairingPSK(persist func(clientID string, psk secure.PSK) error, resulting ServerConfig) (*ServerSession, error) {
	// 1. client/pair-init (parsed for the wire sequence; the pairing index
	//    check uses this session's pairing counter).
	msg, err := readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read client/pair-init: %w", err)
	}
	switch m := msg.(type) {
	case *ClientPairInit:
	case *PairAbort:
		return nil, &PairAbortError{Reason: m.Reason}
	default:
		return nil, fmt.Errorf("expected client/pair-init first, got %T", msg)
	}

	// 2. client/pair-finalize.
	msg, err = readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read client/pair-finalize: %w", err)
	}
	switch m := msg.(type) {
	case *ClientPairFinalize:
		if m.LongTermPSK == "" || m.WrappedPSK != "" {
			return nil, fmt.Errorf("pairing-psk flow requires long_term_psk (and only it)")
		}
		psk, err := secure.ParsePSK(m.LongTermPSK)
		if err != nil {
			return nil, fmt.Errorf("client/pair-finalize: %w", err)
		}
		// The delivered PSK must not equal the Sentinel PSK (protocol error).
		if psk == secure.SentinelPSK() {
			s.Conn.Close()
			return nil, fmt.Errorf("delivered long-term PSK equals the Sentinel PSK (protocol error)")
		}
		if err := persist(s.Conn.PeerID(), psk); err != nil {
			return nil, fmt.Errorf("persist pairing record: %w", err)
		}
		if err := writeMessage(s.Conn, ServerPairFinalize{}); err != nil {
			return nil, fmt.Errorf("send server/pair-finalize: %w", err)
		}
		return s.Promote(psk, resulting)
	case *PairAbort:
		return nil, &PairAbortError{Reason: m.Reason}
	default:
		return nil, fmt.Errorf("expected client/pair-finalize, got %T", msg)
	}
}

// Unpair drops the server's pairing record for this client: it sends
// server/unpair and returns, leaving the connection for the client to close
// (the client removes the record and closes with goodbye 'unpaired'). The
// server-side record removal is the caller's job (it owns the store).
func (s *ServerSession) Unpair() error {
	return writeMessage(s.Conn, ServerUnpair{})
}

// CompletePairingPSKClient runs the client side of the Pairing PSK flow: the
// client MUST send client/pair-init followed immediately by client/pair-finalize
// (without waiting for a server response); then await the server's
// acknowledgement, persist locally, respond to the server's re-handshake, and
// re-establish the session with resultingHello.
func (s *ClientSession) CompletePairingPSKClient(newPSK secure.PSK, persist func(serverID string, psk secure.PSK) error, resultingHello ClientHello) (*ClientSession, error) {
	if err := writeMessage(s.Conn, ClientPairInit{}); err != nil {
		return nil, fmt.Errorf("send client/pair-init: %w", err)
	}
	if err := writeMessage(s.Conn, ClientPairFinalize{LongTermPSK: newPSK.String()}); err != nil {
		return nil, fmt.Errorf("send client/pair-finalize: %w", err)
	}

	msg, err := readControl(s.Conn)
	if err != nil {
		return nil, fmt.Errorf("read server/pair-finalize: %w", err)
	}
	switch m := msg.(type) {
	case *ServerPairFinalize:
	case *PairAbort:
		return nil, &PairAbortError{Reason: m.Reason}
	default:
		return nil, fmt.Errorf("expected server/pair-finalize, got %T", msg)
	}
	if err := persist(s.Conn.PeerID(), newPSK); err != nil {
		return nil, fmt.Errorf("persist pairing record: %w", err)
	}

	// The server now initiates the in-band re-handshake; its first frame is
	// a noise/handshake envelope, followed by a server/activate (no
	// client/hello is re-sent, per the spec's re-handshake rules).
	msgType, payload, err := s.Conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read rehandshake message 1: %w", err)
	}
	if msgType != secure.MsgTypeJSON || !isNoiseHandshake(payload) {
		return nil, fmt.Errorf("expected noise/handshake after pairing, got type %d", msgType)
	}
	if err := s.CompletePromotion(payload, newPSK, resultingHello); err != nil {
		return nil, err
	}
	return s, nil
}

func isNoiseHandshake(raw []byte) bool {
	var env Envelope
	return json.Unmarshal(raw, &env) == nil && env.Type == "noise/handshake"
}
