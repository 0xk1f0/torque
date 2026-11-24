// ABOUTME: End-to-end test of the dynamic pairing-code (PIN) flow through
// Receiver: a Sentinel-keyed peer that demands pairing, the client emitting
// a code the operator enters on the server, promotion to long-term trust with
// the player role, and persistence of the minted long-term PSK.
package sendspin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xk1f0/torque/pkg/protocol/secure"
	"github.com/0xk1f0/torque/pkg/protocol/session"
	"github.com/coder/websocket"
)

// TestReceiver_DynamicPINPairing drives the dynamic pairing-code (PIN) flow
// through the client: a Sentinel-keyed connection whose activate demands a
// pairing code, the client emitting a 6-digit code the server consumes, and
// the resulting long-term PSK persisted so a reconnect can authenticate.
func TestReceiver_DynamicPINPairing(t *testing.T) {
	serverID, _ := secure.GenerateIdentity()
	start := time.Now()
	clock := func() int64 { return time.Since(start).Microseconds() }
	serverStore := session.NewMemoryPairingStore()
	available := make(chan struct{}, 1)
	codeCh := make(chan string, 1)

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// PIN pairing starts Sentinel-keyed: no SelectPSK override.
		conn, _, err := secure.ServerHandshake(ws, secure.ServerHandshakeConfig{Identity: serverID})
		if err != nil {
			return
		}
		sess, err := session.EstablishServer(conn, session.ServerConfig{
			ServerName:        "pin-peer",
			PSKCategory:       session.PSKSentinel,
			InitialActivities: []session.Activity{session.ActivityPairing},
			Pairing: func() *session.Pairing {
				f := session.PairingFormatDigits
				return &session.Pairing{Method: session.PairMethodDynamicPairingCode, Format: &f}
			}(),
		})
		if err != nil {
			return
		}
		after, err := sess.CompletePINPairing(session.PINPairingServerConfig{
			Method:       string(session.PairMethodDynamicPairingCode),
			PairingIndex: 1,
			Format:       string(session.PairingFormatDigits),
			ProvideCode:  func() ([]byte, error) { return []byte(<-codeCh), nil },
			Persist:      serverStore.PutRecord,
			Resulting:    session.ServerConfig{ServerName: "pin-peer", InitialActivities: []session.Activity{session.ActivityPlayback}},
		})
		if err != nil {
			t.Logf("server pin pairing: %v", err)
			return
		}
		for {
			msgType, payload, err := after.Conn.ReadMessage()
			if err != nil {
				return
			}
			if msgType != secure.MsgTypeJSON {
				continue
			}
			msg, err := session.Parse(payload)
			if err != nil {
				continue
			}
			switch v := msg.(type) {
			case *session.ClientTime:
				_ = writeControl(after.Conn, session.ServerTime{
					ClientTransmitted: v.ClientTransmitted,
					ServerReceived:    clock(),
					ServerTransmitted: clock(),
				})
			case *session.ClientState:
				if v.Available {
					select {
					case available <- struct{}{}:
					default:
					}
				}
			case *session.ClientGoodbye:
				return
			}
		}
	}))
	defer hs.Close()

	addr := strings.TrimSuffix(strings.TrimPrefix("ws"+strings.TrimPrefix(hs.URL, "http"), "ws://"), "/")

	clientID, _ := secure.GenerateIdentity()
	store, err := NewFileStore(t.TempDir() + "/pairings.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	emitted := make(chan string, 1)

	recv, err := NewReceiver(Config{
		ServerAddr:         addr,
		Identity:           clientID,
		Name:               "pin-client",
		Store:              store,
		PairingCodeEmitted: func(code string) { emitted <- code },
		MaxSampleRate:      48000,
		MaxBitDepth:        16,
	})
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	defer recv.Close()
	go func() {
		for range recv.Output() {
		}
	}()

	// Forward any emitted code to the server while Connect runs, so the
	// operator "types" it in as soon as torque surfaces it.
	codeSeen := make(chan string, 1)
	go func() {
		select {
		case code := <-emitted:
			codeSeen <- code
			select {
			case codeCh <- code:
			default:
			}
		case <-time.After(5 * time.Second):
		}
	}()

	if err := recv.Connect(); err != nil {
		t.Fatalf("Receiver.Connect (dynamic PIN): %v", err)
	}

	// The client must surface exactly 6 digits.
	select {
	case code := <-codeSeen:
		if len(code) != 6 {
			t.Fatalf("emitted pairing code = %q, want 6 digits", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client never emitted a pairing code")
	}

	// The server must reach the available state after pairing.
	select {
	case <-available:
	case <-time.After(3 * time.Second):
		t.Fatal("peer never saw the paired client report available:true")
	}

	// Exactly one long-term PSK must have been persisted into the unified
	// store.
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var pf persistedStore
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatalf("unmarshal store: %v", err)
	}
	if n := len(pf.Pairings); n != 1 {
		t.Fatalf("store has %d pairing records, want 1", n)
	}
}
