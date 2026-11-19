// ABOUTME: Loopback tests for the encrypted connection: full preamble over a
// ABOUTME: real WebSocket, bidirectional traffic, fragmentation, failure modes.
package secure

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type loopbackResult struct {
	conn *Conn
	ci   ClientInit
	err  error
}

// TestServerError_Classification covers the init-failure reasons per spec:
// version checked before suite, an integer version other than 1 is always
// unsupported_version, and everything else is malformed.
func TestServerError_Classification(t *testing.T) {
	id, _ := GenerateIdentity()
	good := "{\"type\":\"client/init\",\"payload\":{\"client_id\":\"" + id.ID() + "\",\"version\":1,\"suite\":\"25519_ChaChaPoly_SHA256\"}}"

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"valid input classified malformed only on other fields",
			good, ServerErrorMalformed}, // classify runs only on failure; a valid input wouldn't reach it
		{"not JSON", "not json", ServerErrorMalformed},
		{"wrong type", `{"type":"server/init","payload":{}}`, ServerErrorMalformed},
		{"version 2 is unsupported_version", `{"type":"client/init","payload":{"client_id":"x","version":2,"suite":"25519_ChaChaPoly_SHA256"}}`, ServerErrorUnsupportedVersion},
		{"version not an integer is malformed", `{"type":"client/init","payload":{"client_id":"x","version":"1","suite":"25519_ChaChaPoly_SHA256"}}`, ServerErrorMalformed},
		{"unknown suite is unsupported_suite", `{"type":"client/init","payload":{"client_id":"x","version":1,"suite":"25519_DES_MD5"}}`, ServerErrorUnsupportedSuite},
		{"bad client_id is malformed", `{"type":"client/init","payload":{"client_id":"bogus","version":1,"suite":"25519_ChaChaPoly_SHA256"}}`, ServerErrorMalformed},
		{"version precedes suite", `{"type":"client/init","payload":{"client_id":"x","version":2,"suite":"25519_DES_MD5"}}`, ServerErrorUnsupportedVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInitFailure([]byte(tc.raw)); got != tc.want {
				t.Errorf("classifyInitFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestConn_ServerSendsErrorOnBadInit verifies the server emits server/error
// (cleartext, then close) when client/init is rejected, per failure handling.
func TestConn_ServerSendsErrorOnBadInit(t *testing.T) {
	serverID, _ := GenerateIdentity()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, _ := websocket.Accept(w, r, nil)
		_, _, _ = ServerHandshake(ws, ServerHandshakeConfig{Identity: serverID})
		ws.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	bad := `{"type":"client/init","payload":{"client_id":"x","version":2,"suite":"25519_ChaChaPoly_SHA256"}}`
	if err := ws.Write(context.Background(), websocket.MessageText, []byte(bad)); err != nil {
		t.Fatal(err)
	}
	mt, data, err := ws.Read(context.Background())
	if err != nil {
		t.Fatalf("read server/error: %v", err)
	}
	if mt != websocket.MessageText {
		t.Fatalf("server/error must be a text frame, got %d", mt)
	}
	if !strings.Contains(string(data), `"unsupported_version"`) {
		t.Errorf("server/error = %s, want unsupported_version", data)
	}
}

// startLoopbackServer runs an httptest WebSocket endpoint that performs the
// server side of the preamble and hands the resulting Conn to the callback.
func startLoopbackServer(t *testing.T, cfg ServerHandshakeConfig, serve func(*Conn, ClientInit)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		conn, ci, err := ServerHandshake(ws, cfg)
		if err != nil {
			return // handshake failures close the socket; tests observe client side
		}
		serve(conn, ci)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, srv *httptest.Server, cfg ClientHandshakeConfig) (*Conn, ServerInit, error) {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), wsURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return ClientHandshake(ws, cfg)
}

func TestConn_LoopbackBothSuites(t *testing.T) {
	for _, suite := range []Suite{SuiteChaChaPoly, SuiteAESGCM} {
		t.Run(string(suite), func(t *testing.T) {
			serverID, _ := GenerateIdentity()
			clientID, _ := GenerateIdentity()

			artwork := make([]byte, 180_000)
			rand.New(rand.NewSource(7)).Read(artwork)

			srvDone := make(chan error, 1)
			srv := startLoopbackServer(t, ServerHandshakeConfig{Identity: serverID}, func(conn *Conn, ci ClientInit) {
				defer conn.Close()
				srvDone <- func() error {
					if ci.ClientID != clientID.ID() {
						t.Errorf("server saw client_id %q, want %q", ci.ClientID, clientID.ID())
					}
					// server/hello equivalent.
					if err := conn.WriteJSON(map[string]any{"type": "server/hello", "payload": map[string]any{"name": "loopback"}}); err != nil {
						return err
					}
					// Expect a JSON reply from the client.
					msgType, payload, err := conn.ReadMessage()
					if err != nil {
						return err
					}
					if msgType != MsgTypeJSON || !bytes.Contains(payload, []byte("client/hello")) {
						t.Errorf("unexpected client message: type %d payload %.60s", msgType, payload)
					}
					// A fragmented artwork-sized binary message.
					return conn.WriteMessage(8, artwork)
				}()
			})

			conn, si, err := dial(t, srv, ClientHandshakeConfig{
				Identity: clientID,
				Suite:    suite,
			})
			if err != nil {
				t.Fatalf("client handshake: %v", err)
			}
			defer conn.Close()
			if si.ServerID != serverID.ID() {
				t.Errorf("client saw server_id %q, want %q", si.ServerID, serverID.ID())
			}
			if conn.PeerID() != serverID.ID() {
				t.Errorf("PeerID = %q, want server id", conn.PeerID())
			}

			msgType, payload, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read server/hello: %v", err)
			}
			if msgType != MsgTypeJSON || !bytes.Contains(payload, []byte("server/hello")) {
				t.Fatalf("unexpected first message: type %d payload %.60s", msgType, payload)
			}

			if err := conn.WriteJSON(map[string]any{"type": "client/hello", "payload": map[string]any{"name": "loopback-client"}}); err != nil {
				t.Fatalf("write client/hello: %v", err)
			}

			msgType, payload, err = conn.ReadMessage()
			if err != nil {
				t.Fatalf("read artwork: %v", err)
			}
			if msgType != 8 || !bytes.Equal(payload, artwork) {
				t.Errorf("artwork message corrupted: type %d, %d bytes", msgType, len(payload))
			}

			if err := <-srvDone; err != nil {
				t.Fatalf("server side: %v", err)
			}
		})
	}
}

func TestConn_ExpectedServerIDMismatchFails(t *testing.T) {
	serverID, _ := GenerateIdentity()
	clientID, _ := GenerateIdentity()
	other, _ := GenerateIdentity()

	srv := startLoopbackServer(t, ServerHandshakeConfig{Identity: serverID}, func(conn *Conn, _ ClientInit) { conn.Close() })

	_, _, err := dial(t, srv, ClientHandshakeConfig{
		Identity:         clientID,
		ExpectedServerID: other.ID(),
	})
	if err == nil {
		t.Fatal("handshake with mismatched expected server id succeeded")
	}
}

func TestConn_ClientRejectsUnknownPSK(t *testing.T) {
	serverID, _ := GenerateIdentity()
	clientID, _ := GenerateIdentity()
	longTerm, _ := NewPSK()

	// Server offers a long-term PSK the client does not have. With the
	// initial-handshake sentinel fallback the client completes message 2 with
	// the Sentinel PSK (client-side success); this server's handshake state is
	// bound to the long-term PSK (flynn/noise fixes the PSK at state creation,
	// so the server-side sentinel re-verify is not expressible) and then drops
	// the connection. The fallback mechanics are covered by
	// TestResponder_SentinelFallback.
	srv := startLoopbackServer(t, ServerHandshakeConfig{
		Identity: serverID,
		SelectPSK: func(string) (PSK, PSKCategoryCode, error) {
			return longTerm, CategoryLongTerm, nil
		},
	}, func(conn *Conn, _ ClientInit) { conn.Close() })

	if _, _, err := dial(t, srv, ClientHandshakeConfig{Identity: clientID}); err != nil {
		t.Fatalf("client should complete message 2 via sentinel fallback, got %v", err)
	}

	// Disabling the fallback (re-handshake semantics) must fail the handshake
	// on a lookup miss.
	noFallback := false
	_, _, err := dial(t, srv, ClientHandshakeConfig{Identity: clientID, SentinelFallback: &noFallback})
	if err == nil {
		t.Fatal("handshake without sentinel fallback succeeded on a lookup miss")
	}
}

// TestResponder_SentinelFallback verifies the client completes message 2 with
// the Sentinel PSK on an initial-handshake psk_id lookup miss, producing a
// usable (sentinel-keyed) session — the lost-record recovery path.
func TestResponder_SentinelFallback(t *testing.T) {
	server, client, prologue := handshakePeers(t, SuiteChaChaPoly)
	longTerm, _ := NewPSK()

	init, _ := NewInitiator(InitiatorConfig{
		Identity: server, PeerPublicKey: client.PublicKey(),
		Suite: SuiteChaChaPoly, Prologue: prologue, PSK: longTerm,
	})
	msg1, err := init.Message1(longTerm.ID(), CategoryLongTerm)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := NewResponder(ResponderConfig{
		Identity: client, PeerPublicKey: server.PublicKey(),
		Suite: SuiteChaChaPoly, Prologue: prologue,
		// The client has no record for this psk_id → miss.
		SelectPSK: func(string, PSKCategoryCode) (PSK, bool) { return PSK{}, false },
		// Initial handshake: fall back to the sentinel.
		SentinelFallback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resp.Accept(msg1); err != nil {
		t.Fatalf("Accept with sentinel fallback: %v", err)
	}

	// Disabled fallback (re-handshake behavior): a miss must fail.
	respNoFB, _ := NewResponder(ResponderConfig{
		Identity: client, PeerPublicKey: server.PublicKey(),
		Suite: SuiteChaChaPoly, Prologue: prologue,
		SelectPSK: func(string, PSKCategoryCode) (PSK, bool) { return PSK{}, false },
	})
	if _, _, err := respNoFB.Accept(msg1); err == nil {
		t.Error("Accept without sentinel fallback succeeded on a lookup miss")
	}
}

func TestConn_HandshakeTimeout(t *testing.T) {
	// A server that upgrades but never speaks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		time.Sleep(5 * time.Second)
		ws.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	clientID, _ := GenerateIdentity()
	ws, _, err := websocket.Dial(context.Background(), wsURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	start := time.Now()
	_, _, err = ClientHandshake(ws, ClientHandshakeConfig{
		Identity:         clientID,
		HandshakeTimeout: 300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("handshake against a silent server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout took %v, want ~300ms", elapsed)
	}
}

func TestConn_TextFrameInTransportModeFails(t *testing.T) {
	serverID, _ := GenerateIdentity()
	clientID, _ := GenerateIdentity()

	srv := startLoopbackServer(t, ServerHandshakeConfig{Identity: serverID}, func(conn *Conn, _ ClientInit) {
		// Violate the spec: send a text frame after transport mode.
		conn.ws.Write(context.Background(), websocket.MessageText, []byte(`{"type":"oops"}`))
		time.Sleep(200 * time.Millisecond)
		conn.Close()
	})

	conn, _, err := dial(t, srv, ClientHandshakeConfig{Identity: clientID})
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	defer conn.Close()
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("text frame in transport mode accepted")
	}
}

// TestConn_WriteSurvivesHandshakeTimeout guards the clearing of the handshake's
// write deadline. The preamble sets a per-message write deadline equal to the
// handshake timeout; if it is left in place after transport mode starts, every
// later write silently fails with an i/o timeout once it lapses (invalidating
// all long-lived outbound traffic — pairing, state, stream data) while the read
// side sees no error and so never reconnects. A message written well past that
// deadline must still be delivered.
func TestConn_WriteSurvivesHandshakeTimeout(t *testing.T) {
	serverID, _ := GenerateIdentity()
	clientID, _ := GenerateIdentity()

	const shortTO = 50 * time.Millisecond

	srvDone := make(chan error, 1)
	srv := startLoopbackServer(t, ServerHandshakeConfig{
		Identity:         serverID,
		HandshakeTimeout: shortTO,
	}, func(conn *Conn, _ ClientInit) {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			srvDone <- err
			return
		}
		if msgType != MsgTypeJSON || !bytes.Contains(payload, []byte("post-timeout")) {
			srvDone <- fmt.Errorf("unexpected post-timeout message: type=%d payload=%q", msgType, payload)
			return
		}
		srvDone <- nil
	})

	conn, _, err := dial(t, srv, ClientHandshakeConfig{
		Identity:         clientID,
		HandshakeTimeout: shortTO,
	})
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	defer conn.Close()

	// Wait comfortably past the handshake write deadline: a stale deadline would
	// make this write fail immediately with an i/o timeout.
	time.Sleep(shortTO * 5)

	if err := conn.WriteJSON(map[string]any{"type": "client/ping", "payload": map[string]any{"tag": "post-timeout"}}); err != nil {
		t.Fatalf("write after handshake deadline lapsed failed (stale write deadline still set?): %v", err)
	}

	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the post-timeout message")
	}
}
