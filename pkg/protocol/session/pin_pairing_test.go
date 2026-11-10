// ABOUTME: Go-to-Go loopback tests for the PIN pairing flows — dynamic
// ABOUTME: (digits), static, full round trip plus wrong-code failures.
package session

import (
	"testing"

	"github.com/0xk1f0/spinsync/pkg/protocol/secure"
)

// pinPairingHarness runs a server-side and client-side PIN attempt against
// each other over an encrypted loopback keyed on the Sentinel PSK (PIN
// pairing starts Sentinel-keyed), driving each side in its own goroutine.
type pinResult struct {
	serverSess *ServerSession
	serverErr  error
	clientSess *ClientSession
	clientErr  error
}

func runPINPairing(t *testing.T, serverCfg PINPairingServerConfig, clientCfg PINPairingClientConfig, clientHello ClientHello) pinResult {
	t.Helper()
	serverConn, clientConn := loopback(t)

	res := make(chan pinResult, 1)
	srvOut := make(chan struct {
		s *ServerSession
		e error
	}, 1)

	go func() {
		pairing := &Pairing{Method: PairingMethod(serverCfg.Method)}
		if serverCfg.Method == string(PairMethodDynamicPairingCode) {
			f := PairingFormat(serverCfg.Format)
			pairing.Format = &f
		}
		sess, err := EstablishServer(serverConn, ServerConfig{
			ServerName:        "pin-server",
			PSKCategory:       PSKSentinel,
			InitialActivities: []Activity{ActivityPairing},
			Pairing:           pairing,
		})
		if err != nil {
			srvOut <- struct {
				s *ServerSession
				e error
			}{nil, err}
			return
		}
		after, err := sess.CompletePINPairing(serverCfg)
		srvOut <- struct {
			s *ServerSession
			e error
		}{after, err}
	}()

	clientSess, err := EstablishClient(clientConn, ClientConfig{Hello: clientHello, PSKCategory: PSKSentinel})
	var out pinResult
	if err != nil {
		out.clientErr = err
	} else {
		after, perr := clientSess.RunPINPairing(clientCfg)
		out.clientSess, out.clientErr = after, perr
	}
	srv := <-srvOut
	out.serverSess, out.serverErr = srv.s, srv.e
	res <- out
	return <-res
}

func pinClientHello(methods ...string) ClientHello {
	h := testHello(false)
	spm := SupportedPairMethods{}
	for _, m := range methods {
		spm[m] = PairMethodDescriptor{}
	}
	h.SupportedPairMethods = spm
	return h
}

// digitsCodePair wires a server ProvideCode and a client EmitCode around the
// emitted 6-digit code so the operator "types" it into the server.
func digitsCodePair() (provide func() ([]byte, error), emit func(string)) {
	ch := make(chan []byte, 1)
	return func() ([]byte, error) { return <-ch, nil },
		func(code string) { ch <- []byte(code) }
}

func TestPINPairing_DynamicDigitsFullRoundTrip(t *testing.T) {
	clientStore := NewMemoryPairingStore()
	serverStore := NewMemoryPairingStore()
	resultingHello := testHello(true)

	provide, emit := digitsCodePair()
	res := runPINPairing(t,
		PINPairingServerConfig{
			Method:       string(PairMethodDynamicPairingCode),
			PairingIndex: 1,
			Format:       string(PairingFormatDigits),
			ProvideCode:  provide,
			Persist:      serverStore.PutRecord,
			Resulting:    ServerConfig{ServerName: "pin-server", InitialActivities: []Activity{ActivityPlayback}},
		},
		PINPairingClientConfig{
			Method:         string(PairMethodDynamicPairingCode),
			PairingIndex:   1,
			Format:         string(PairingFormatDigits),
			EmitCode:       emit,
			Persist:        clientStore.PutRecord,
			ResultingHello: resultingHello,
		},
		pinClientHello(string(PairMethodDynamicPairingCode)),
	)

	if res.serverErr != nil {
		t.Fatalf("server: %v", res.serverErr)
	}
	if res.clientErr != nil {
		t.Fatalf("client: %v", res.clientErr)
	}
	assertPairedBothSides(t, res, serverStore, clientStore)
}

func TestPINPairing_DynamicQRFullRoundTrip(t *testing.T) {
	clientStore := NewMemoryPairingStore()
	serverStore := NewMemoryPairingStore()
	resultingHello := testHello(true)

	ch := make(chan []byte, 1)
	res := runPINPairing(t,
		PINPairingServerConfig{
			Method:       string(PairMethodDynamicPairingCode),
			PairingIndex: 1,
			Format:       string(PairingFormatQR),
			ProvideCode:  func() ([]byte, error) { return <-ch, nil },
			Persist:      serverStore.PutRecord,
			Resulting:    ServerConfig{ServerName: "pin-server", InitialActivities: []Activity{ActivityPlayback}},
		},
		PINPairingClientConfig{
			Method:       string(PairMethodDynamicPairingCode),
			PairingIndex: 1,
			Format:       string(PairingFormatQR),
			// The client emits the SP: v1 token; the operator "scans" it by
			// decoding it back to the 24-byte code before typing it in.
			EmitCode: func(tok string) {
				_, payload, err := secure.DecodePairingToken(tok)
				if err != nil {
					t.Errorf("client emitted malformed token: %v", err)
					return
				}
				ch <- payload
			},
			Persist:        clientStore.PutRecord,
			ResultingHello: resultingHello,
		},
		pinClientHello(string(PairMethodDynamicPairingCode)),
	)

	if res.serverErr != nil {
		t.Fatalf("server: %v", res.serverErr)
	}
	if res.clientErr != nil {
		t.Fatalf("client: %v", res.clientErr)
	}
	assertPairedBothSides(t, res, serverStore, clientStore)
}

func TestPINPairing_StaticFullRoundTrip(t *testing.T) {
	const staticCode = "13572468"
	clientStore := NewMemoryPairingStore()
	serverStore := NewMemoryPairingStore()
	resultingHello := testHello(true)

	res := runPINPairing(t,
		PINPairingServerConfig{
			Method:       string(PairMethodStaticPairingCode),
			PairingIndex: 1,
			ProvideCode:  func() ([]byte, error) { return []byte(staticCode), nil },
			Persist:      serverStore.PutRecord,
			Resulting:    ServerConfig{ServerName: "pin-server", InitialActivities: []Activity{ActivityPlayback}},
		},
		PINPairingClientConfig{
			Method:         string(PairMethodStaticPairingCode),
			PairingIndex:   1,
			StaticCode:     staticCode,
			Persist:        clientStore.PutRecord,
			ResultingHello: resultingHello,
		},
		pinClientHello(string(PairMethodStaticPairingCode)),
	)

	if res.serverErr != nil {
		t.Fatalf("server: %v", res.serverErr)
	}
	if res.clientErr != nil {
		t.Fatalf("client: %v", res.clientErr)
	}
	assertPairedBothSides(t, res, serverStore, clientStore)
}

func TestPINPairing_WrongStaticCodeAborts(t *testing.T) {
	res := runPINPairing(t,
		PINPairingServerConfig{
			Method:       string(PairMethodStaticPairingCode),
			PairingIndex: 1,
			ProvideCode:  func() ([]byte, error) { return []byte("11111111"), nil }, // operator types wrong
			Persist:      NewMemoryPairingStore().PutRecord,
			Resulting:    ServerConfig{ServerName: "pin-server", InitialActivities: []Activity{ActivityPlayback}},
		},
		PINPairingClientConfig{
			Method:         string(PairMethodStaticPairingCode),
			PairingIndex:   1,
			StaticCode:     "99999999", // device's real code
			Persist:        NewMemoryPairingStore().PutRecord,
			ResultingHello: testHello(false),
		},
		pinClientHello(string(PairMethodStaticPairingCode)),
	)

	// The client verifies server_kc first and fails, aborting with the spec's
	// pairing_code_mismatch; the server sees the abort.
	assertPairAbort(t, res.clientErr, AbortPairingCodeMismatch)
	if res.serverErr == nil {
		t.Error("server completed pairing with a mismatched code")
	}
}

func assertPairedBothSides(t *testing.T, res pinResult, serverStore, clientStore *MemoryPairingStore) {
	t.Helper()
	if res.serverSess == nil || res.clientSess == nil {
		t.Fatal("pairing produced no post-session")
	}
	sr := serverStore.Records()
	cr := clientStore.Records()
	if len(sr) != 1 || len(cr) != 1 {
		t.Fatalf("records: server=%d client=%d", len(sr), len(cr))
	}
	if sr[0].PSK != cr[0].PSK {
		t.Error("server and client persisted different PSKs")
	}
	// The re-keyed channel carries traffic.
	if err := res.clientSess.SendGoodbye(GoodbyeShutdown); err != nil {
		t.Fatal(err)
	}
	if _, _, err := res.serverSess.Conn.ReadMessage(); err != nil {
		t.Fatalf("post-pairing read: %v", err)
	}
}

func assertPairAbort(t *testing.T, err error, reason string) {
	t.Helper()
	abort, ok := err.(*PairAbortError)
	if !ok {
		t.Fatalf("error = %v, want *PairAbortError(%s)", err, reason)
	}
	if abort.Reason != reason {
		t.Errorf("abort reason = %q, want %q", abort.Reason, reason)
	}
}

// The dynamic digits derivation must be byte- and code-stable across both
// sides for several handshake hashes (guards the label/sid/derive plumbing).
func TestPINPairing_DynamicDigitsStableAcrossHashes(t *testing.T) {
	for round := uint32(1); round <= 2; round++ {
		serverStore := NewMemoryPairingStore()
		clientStore := NewMemoryPairingStore()
		provide, emit := digitsCodePair()
		res := runPINPairing(t,
			PINPairingServerConfig{
				Method: string(PairMethodDynamicPairingCode), PairingIndex: round, Format: string(PairingFormatDigits),
				ProvideCode: provide,
				Persist:     serverStore.PutRecord,
				Resulting:   ServerConfig{ServerName: "s", InitialActivities: []Activity{ActivityPlayback}},
			},
			PINPairingClientConfig{
				Method: string(PairMethodDynamicPairingCode), PairingIndex: round, Format: string(PairingFormatDigits),
				EmitCode:       emit,
				Persist:        clientStore.PutRecord,
				ResultingHello: testHello(true),
			},
			pinClientHello(string(PairMethodDynamicPairingCode)),
		)
		if res.serverErr != nil || res.clientErr != nil {
			t.Fatalf("index %d: server=%v client=%v", round, res.serverErr, res.clientErr)
		}
		if serverStore.Records()[0].PSK != clientStore.Records()[0].PSK {
			t.Errorf("index %d: PSK mismatch", round)
		}
	}
}
