// ABOUTME: Loopback tests for the Pairing PSK flow — full round trip with
// ABOUTME: re-handshake and trust promotion, plus store resolve order.
package session

import (
	"testing"

	"github.com/Sendspin/sendspin-go/pkg/protocol/secure"
)

func TestMemoryPairingStore_ResolveOrder(t *testing.T) {
	store := NewMemoryPairingStore()
	longTerm, _ := secure.NewPSK()
	pairing, _ := secure.NewPSK()

	if _, cat := store.Resolve("c1"); cat != PSKSentinel {
		t.Errorf("empty store resolved %v, want sentinel", cat)
	}

	store.StagePairingPSK("c1", pairing)
	if psk, cat := store.Resolve("c1"); cat != PSKPairing || psk != pairing {
		t.Errorf("staged resolve = (%v, %v)", psk, cat)
	}

	if err := store.PutRecord("c1", longTerm); err != nil {
		t.Fatal(err)
	}
	if psk, cat := store.Resolve("c1"); cat != PSKLongTerm || psk != longTerm {
		t.Errorf("record resolve = (%v, %v)", psk, cat)
	}
	// PutRecord clears the staged entry.
	if len(store.staged) != 0 {
		t.Error("staged pairing psk not cleared after PutRecord")
	}
}

// pairedLoopback builds an encrypted loopback where the handshake used the
// given PSK (staged Pairing PSK on both sides).
func pairedLoopback(t *testing.T, pairingPSK secure.PSK) (serverConn, clientConn *secure.Conn) {
	t.Helper()
	serverID, _ := secure.GenerateIdentity()
	clientID, _ := secure.GenerateIdentity()

	srvConnCh := make(chan *secure.Conn, 1)
	srv := startSecureLoopback(t, serverID, func(string) (secure.PSK, secure.PSKCategoryCode, error) {
		return pairingPSK, secure.CategoryPairing, nil
	}, srvConnCh)

	clientConn = dialSecure(t, srv, clientID, func(pskID string, _ secure.PSKCategoryCode) (secure.PSK, bool) {
		if pskID == pairingPSK.ID() {
			return pairingPSK, true
		}
		return secure.PSK{}, false
	})
	return <-srvConnCh, clientConn
}

func TestPairingPSK_FullRoundTrip(t *testing.T) {
	pairingPSK, _ := secure.NewPSK()
	serverConn, clientConn := pairedLoopback(t, pairingPSK)
	defer serverConn.Close()
	defer clientConn.Close()

	serverStore := NewMemoryPairingStore()
	newSessCh := make(chan *ServerSession, 1)
	errCh := make(chan error, 2)

	go func() {
		sess, err := EstablishServer(serverConn, ServerConfig{
			ServerName:        "pair-server",
			PSKCategory:       PSKPairing,
			InitialActivities: []Activity{ActivityPairing},
			Pairing:           &Pairing{Method: PairMethodPairingPSK},
		})
		if err != nil {
			errCh <- err
			return
		}
		if len(sess.ActiveRoles) != 0 {
			t.Errorf("pairing activate carried roles %v", sess.ActiveRoles)
		}
		after, err := sess.CompletePairingPSK(serverStore.PutRecord, ServerConfig{
			ServerName:        "pair-server",
			InitialActivities: []Activity{ActivityPlayback},
		})
		if err != nil {
			errCh <- err
			return
		}
		newSessCh <- after
	}()

	hello := testHello(false)
	hello.SupportedPairMethods = SupportedPairMethods{string(PairMethodPairingPSK): {}}
	clientSess, err := EstablishClient(clientConn, ClientConfig{
		Hello:       hello,
		PSKCategory: PSKPairing,
	})
	if err != nil {
		t.Fatalf("client establish (pairing): %v", err)
	}
	if clientSess.Activate.Pairing.Method != PairMethodPairingPSK {
		t.Errorf("selected method = %q", clientSess.Activate.Pairing.Method)
	}

	longTerm, _ := secure.NewPSK()
	clientStore := NewMemoryPairingStore()
	resultingHello := testHello(false)
	afterClient, err := clientSess.CompletePairingPSKClient(longTerm, clientStore.PutRecord, resultingHello)
	if err != nil {
		t.Fatalf("client pairing: %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("server side: %v", err)
	case afterServer := <-newSessCh:
		// Both sides persisted the same record.
		srvRecords := serverStore.Records()
		if len(srvRecords) != 1 || srvRecords[0].PSK != longTerm {
			t.Errorf("server records = %+v", srvRecords)
		}
		cliRecords := clientStore.Records()
		if len(cliRecords) != 1 || cliRecords[0].PSK != longTerm {
			t.Errorf("client records = %+v", cliRecords)
		}
		// The re-established session is at long-term trust with playback
		// (roles only exist on a playback-capable, i.e. long-term, session).
		if len(afterServer.ActiveRoles) == 0 {
			t.Error("post-pairing session activated no roles")
		}
		if len(afterClient.ActiveRoles) == 0 {
			t.Error("client post-pairing session has no roles")
		}
		// Traffic flows on the rekeyed channel.
		if err := afterClient.SendGoodbye(GoodbyeShutdown); err != nil {
			t.Fatal(err)
		}
		if _, _, err := afterServer.Conn.ReadMessage(); err != nil {
			t.Fatalf("post-pairing read: %v", err)
		}
	}
}

func TestPairingPSK_ClientAbortSurfaces(t *testing.T) {
	pairingPSK, _ := secure.NewPSK()
	serverConn, clientConn := pairedLoopback(t, pairingPSK)
	defer serverConn.Close()
	defer clientConn.Close()

	errCh := make(chan error, 1)
	go func() {
		sess, err := EstablishServer(serverConn, ServerConfig{
			ServerName:        "pair-server",
			PSKCategory:       PSKPairing,
			InitialActivities: []Activity{ActivityPairing},
			Pairing:           &Pairing{Method: PairMethodPairingPSK},
		})
		if err != nil {
			errCh <- err
			return
		}
		_, err = sess.CompletePairingPSK(NewMemoryPairingStore().PutRecord, ServerConfig{
			ServerName:        "pair-server",
			InitialActivities: []Activity{ActivityPlayback},
		})
		errCh <- err
	}()

	hello := testHello(false)
	hello.SupportedPairMethods = SupportedPairMethods{string(PairMethodPairingPSK): {}}
	clientSess, err := EstablishClient(clientConn, ClientConfig{Hello: hello, PSKCategory: PSKPairing})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMessage(clientSess.Conn, PairAbort{Reason: "user_cancelled"}); err != nil {
		t.Fatal(err)
	}

	err = <-errCh
	abort, ok := err.(*PairAbortError)
	if !ok || abort.Reason != "user_cancelled" {
		t.Fatalf("server error = %v, want PairAbortError(user_cancelled)", err)
	}
}
