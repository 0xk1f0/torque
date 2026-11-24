// ABOUTME: End-to-end tests of the spec-v2 client (ReceiverV2/PlayerV2) against
// ABOUTME: a minimal encrypted test peer built from the transport + session
// ABOUTME: primitives — handshake, establish, clock sync, stream lifecycle.
package sendspin

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0xk1f0/torque/pkg/protocol/playback"
	"github.com/0xk1f0/torque/pkg/protocol/secure"
	"github.com/0xk1f0/torque/pkg/protocol/session"
	syncc "github.com/0xk1f0/torque/pkg/sync"
	"github.com/coder/websocket"
	"slices"
)

// v2TestPeer is a minimal encrypted Sendspin server built purely so the client
// has someone to talk to: Noise server handshake, session establishment, and a
// control loop that answers clock sync and, once the client reports available,
// streams a short PCM tone. It exists only for tests; there is no server
// product in this repository.
type v2TestPeer struct {
	hs *httptest.Server
}

// newV2TestPeer starts a peer the client is already paired with: the Noise
// handshake is keyed on a shared long-term PSK and playback is activated
// directly (no pairing needed). onAvailable receives true the first time the
// client reports available:true.
func newV2TestPeer(t *testing.T, longTerm secure.PSK, onAvailable func()) *v2TestPeer {
	t.Helper()
	serverID, err := secure.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	clock := func() int64 { return time.Since(start).Microseconds() }

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Origin"), "http") {
			t.Errorf("server accepted an Origin header; spec forbids it")
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		handlePeerConn(t, ws, serverID, longTerm, clock, onAvailable)
	}))
	t.Cleanup(hs.Close)
	return &v2TestPeer{hs: hs}
}

func (p *v2TestPeer) url() string {
	return "ws" + strings.TrimPrefix(p.hs.URL, "http")
}

// streamPCM writes nFrames of 20 ms constant PCM to conn at the given format.
// It runs on the peer's own goroutine, so writes are non-fatal: the client
// may close early (e.g. the test finished), which simply stops the stream.
func streamPCM(conn *secure.Conn, clock func() int64, nFrames int, value int32) {
	chunkSamples := 48000 * 20 / 1000 * 2
	ts := clock() + 100_000 // 100 ms send-ahead
	if err := writeControl(conn, session.GroupUpdate{
		PlaybackState: session.PlaybackPlaying,
		GroupID:       "gp-tx",
		GroupName:     "peer",
	}); err != nil {
		return
	}
	if err := writeControl(conn, session.StreamStart{
		ServerTransmitted: clock(),
		Player: &session.StreamStartPlayer{
			Codec: "pcm", Channels: 2, SampleRate: 48000, BitDepth: 16,
		},
	}); err != nil {
		return
	}
	for i := 0; i < nFrames; i++ {
		body := make([]byte, chunkSamples*2)
		for j := 0; j < chunkSamples; j++ {
			v := int16(value >> 8) // 16-bit PCM, top 16 bits like the SDK encoder
			binary.LittleEndian.PutUint16(body[j*2:], uint16(v))
		}
		frame := playback.EncodeAudioChunk(ts, ts-100_000, body)
		if err := conn.WriteMessage(playback.PlayerAudioMsgType, frame); err != nil {
			return
		}
		ts += 20_000
		time.Sleep(2 * time.Millisecond)
	}
	_ = writeControl(conn, session.StreamEnd{Roles: []string{"player"}})
	_ = writeControl(conn, session.GroupUpdate{
		PlaybackState: session.PlaybackStopped,
		GroupID:       "gp-tx",
		GroupName:     "peer",
	})
}

func handlePeerConn(t *testing.T, ws *websocket.Conn, serverID *secure.Identity, longTerm secure.PSK, clock func() int64, onAvailable func()) {
	conn, _, err := secure.ServerHandshake(ws, secure.ServerHandshakeConfig{
		Identity: serverID,
		SelectPSK: func(string) (secure.PSK, secure.PSKCategoryCode, error) {
			return longTerm, secure.CategoryLongTerm, nil
		},
	})
	if err != nil {
		return
	}
	sess, err := session.EstablishServer(conn, session.ServerConfig{
		ServerName:        "v2-test-peer",
		PSKCategory:       session.PSKLongTerm,
		InitialActivities: []session.Activity{session.ActivityPlayback},
		ChooseActiveRoles: func(session.ClientHello) []string { return []string{"player@v1"} },
	})
	if err != nil {
		return
	}
	_ = sess

	for {
		msgType, payload, err := conn.ReadMessage()
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
			_ = writeControl(conn, session.ServerTime{
				ClientTransmitted: v.ClientTransmitted,
				ServerReceived:    clock(),
				ServerTransmitted: clock(),
			})
		case *session.ClientState:
			if v.Available {
				if onAvailable != nil {
					onAvailable()
				}
				// Stream a short tone and end the session.
				go streamPCM(conn, clock, 150, 1000)
				return
			}
		case *session.ClientGoodbye:
			return
		}
	}
}

// writeControl marshals and writes a control message. Returns the error so
// callers on the peer's goroutines can bail out non-fatally.
func writeControl(conn *secure.Conn, v any) error {
	raw, err := session.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(secure.MsgTypeJSON, raw)
}

// TestReceiverV2_EndToEndAudio connects ReceiverV2 to the encrypted test peer,
// which streams a constant tone once the client reports available, and verifies
// decoded buffers arrive with correct constants, sample counts, and ~20 ms
// spaced timestamps.
func peerAddr(peer *v2TestPeer) string {
	return strings.TrimSuffix(strings.TrimPrefix(peer.url(), "ws://"), "/")
}

func TestReceiverV2_EndToEndAudio(t *testing.T) {
	longTerm, err := secure.NewPSK()
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(t.TempDir() + "/pairings.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.PutRecord(longTerm); err != nil {
		t.Fatalf("seed pairing store: %v", err)
	}

	peer := newV2TestPeer(t, longTerm, nil)
	addr := peerAddr(peer)

	clientID, _ := secure.GenerateIdentity()
	recv, err := NewReceiverV2(V2Config{
		ServerAddr:     addr,
		Identity:       clientID,
		Name:           "v2-loopback-client",
		Store:          store,
		BufferMs:       100,
		BufferCapacity: 1 << 20,
		MaxSampleRate:  48000,
		MaxBitDepth:    16,
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recv.Close()

	type sample struct {
		ts    int64
		first int32
		count int
	}
	samples := make(chan sample, 512)
	go func() {
		for buf := range recv.Output() {
			samples <- sample{ts: buf.Timestamp, first: buf.Samples[0], count: len(buf.Samples)}
		}
	}()

	if err := recv.Connect(); err != nil {
		t.Fatalf("ReceiverV2.Connect: %v", err)
	}

	var got []sample
	deadline := time.After(5 * time.Second)
	for len(got) < 5 {
		select {
		case sm := <-samples:
			got = append(got, sm)
		case <-deadline:
			t.Fatalf("only %d decoded chunks in 5s", len(got))
		}
	}

	for i := 1; i < len(got); i++ {
		delta := got[i].ts - got[i-1].ts
		if delta < 18_000 || delta > 22_000 {
			t.Errorf("chunk %d: spacing %dµs, want ~20000µs", i, delta)
		}
	}
	if got[0].first == 0 {
		t.Errorf("decoded sample = 0, want a constant non-zero value")
	}
	for i := 1; i < len(got); i++ {
		if got[i].first != got[0].first {
			t.Errorf("chunk %d sample = %d, want constant %d", i, got[i].first, got[0].first)
		}
	}
	if got[0].count != 1920 { // 48000 * 0.02s * 2ch
		t.Errorf("chunk sample count = %d, want 1920", got[0].count)
	}
}

// TestReceiverV2_SyncAndAvailability verifies the client completes clock sync
// with the peer and that its availability reaches the peer (the precondition
// for streaming).
func TestReceiverV2_SyncAndAvailability(t *testing.T) {
	available := make(chan struct{}, 1)
	longTerm, _ := secure.NewPSK()
	store, err := NewFileStore(t.TempDir() + "/pairings.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.PutRecord(longTerm); err != nil {
		t.Fatalf("seed pairing store: %v", err)
	}
	peer := newV2TestPeer(t, longTerm, func() { available <- struct{}{} })
	addr := peerAddr(peer)

	clientID, _ := secure.GenerateIdentity()
	recv, _ := NewReceiverV2(V2Config{
		ServerAddr:    addr,
		Identity:      clientID,
		Name:          "v2-sync-client",
		Store:         store,
		MaxSampleRate: 48000,
		MaxBitDepth:   16,
	})
	defer recv.Close()
	go func() {
		for range recv.Output() {
		}
	}()

	if err := recv.Connect(); err != nil {
		t.Fatalf("ReceiverV2.Connect: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		rtt, _ := recv.ClockSync().GetStats()
		return rtt > 0
	}, "clock sync to produce a sample")

	select {
	case <-available:
	case <-time.After(3 * time.Second):
		t.Fatal("peer never saw the client report available:true")
	}
}

// TestPlayerV2_VolumeMuteRoundTrip verifies local volume/mute state changes
// land in the player state.
func TestPlayerV2_VolumeMuteRoundTrip(t *testing.T) {
	longTerm, _ := secure.NewPSK()
	store, err := NewFileStore(t.TempDir() + "/pairings.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.PutRecord(longTerm); err != nil {
		t.Fatalf("seed pairing store: %v", err)
	}
	peer := newV2TestPeer(t, longTerm, nil)
	addr := peerAddr(peer)

	clientID, _ := secure.GenerateIdentity()
	p, err := NewPlayerV2(V2Config{
		ServerAddr:    addr,
		Identity:      clientID,
		Name:          "v2-player",
		Store:         store,
		MaxSampleRate: 48000,
		MaxBitDepth:   16,
		OnError:       func(error) {},
	})
	if err != nil {
		t.Fatalf("NewPlayerV2: %v", err)
	}
	defer p.Close()

	if err := p.Connect(); err != nil {
		t.Fatalf("PlayerV2.Connect: %v", err)
	}
	if err := p.SetVolume(40); err != nil {
		t.Fatal(err)
	}
	if st := p.Status(); st.Volume != 40 {
		t.Errorf("volume = %d, want 40", st.Volume)
	}
	if err := p.Mute(true); err != nil {
		t.Fatal(err)
	}
	if st := p.Status(); !st.Muted {
		t.Error("muted should be true")
	}
}

// TestV2Client_HelloOffersSpecValidDynamicPairing guards against a pairing
// advertisement a strict server would not recognize. Per the Sendspin spec a
// dynamic_pairing_code descriptor is only recognized when it declares both
// out_channels and (non-empty) formats; lacking out_channels, a server
// (e.g. Home Assistant / Music Assistant) treats the method as unrecognized
// and never requests dynamic pairing.
func TestV2Client_HelloOffersSpecValidDynamicPairing(t *testing.T) {
	clientID, _ := secure.GenerateIdentity()
	recv, err := NewReceiverV2(V2Config{
		ServerAddr: "127.0.0.1:9",
		Identity:   clientID,
		Name:       "hello-test",
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recv.Close()

	hello, _ := recv.buildHello()
	desc, ok := hello.SupportedPairMethods[string(session.PairMethodDynamicPairingCode)]
	if !ok {
		t.Fatal("hello must offer the dynamic_pairing_code method")
	}
	if len(desc.OutChannels) == 0 {
		t.Error("dynamic_pairing_code descriptor must declare out_channels (spec: display|speaker), else the method is treated as unrecognized")
	}
	if len(desc.Formats) == 0 {
		t.Error("dynamic_pairing_code descriptor must declare non-empty formats (spec: digits|qr_code)")
	}
}

// TestV2Client_UnpairedAccessAdvertised guards the --unpaired-access wiring:
// the V2Config.UnpairedAccess knob must reach the wire as the client/hello
// unpaired_access flag. Off (the zero value, and the CLI default) advertises
// enabled:false so a fresh client still requires dynamic pin-code pairing;
// on advertises enabled:true so an unpaired server may open direct playback.
func TestV2Client_UnpairedAccessAdvertised(t *testing.T) {
	clientID, _ := secure.GenerateIdentity()

	recvOff, err := NewReceiverV2(V2Config{
		ServerAddr:     "127.0.0.1:9",
		Identity:       clientID,
		Name:           "unpaired-off",
		UnpairedAccess: false, // explicit; also the zero-value default
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recvOff.Close()
	if hello, _ := recvOff.buildHello(); hello.UnpairedAccess.Enabled {
		t.Error("UnpairedAccess=false must advertise unpaired_access.enabled=false")
	}

	recvOn, err := NewReceiverV2(V2Config{
		ServerAddr:     "127.0.0.1:9",
		Identity:       clientID,
		Name:           "unpaired-on",
		UnpairedAccess: true,
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recvOn.Close()
	if hello, _ := recvOn.buildHello(); !hello.UnpairedAccess.Enabled {
		t.Error("UnpairedAccess=true must advertise unpaired_access.enabled=true")
	}
}

// TestReceiverV2_EmptyActivateStaysConnected guards the fix for servers (e.g.
// Home Assistant / Music Assistant) that hold an unpaired connection at an
// empty server/activate — no roles, no pairing — until an operator drives
// pairing or role activation. Connect must NOT treat that as a fatal error:
// it returns successfully and the connection stays open awaiting a later
// server/activate.
func TestReceiverV2_EmptyActivateStaysConnected(t *testing.T) {
	serverID, _ := secure.GenerateIdentity()
	clientDone := make(chan struct{})

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn, _, err := secure.ServerHandshake(ws, secure.ServerHandshakeConfig{Identity: serverID})
		if err != nil {
			return
		}
		// Initial activate: empty activities, no roles, no pairing.
		if _, err := session.EstablishServer(conn, session.ServerConfig{
			ServerName:        "v2-empty-peer",
			PSKCategory:       session.PSKSentinel,
			InitialActivities: nil,
			ChooseActiveRoles: func(session.ClientHello) []string { return nil },
		}); err != nil {
			return
		}
		// Stay connected (and respond to clock sync) until the client leaves.
		for {
			msgType, payload, err := conn.ReadMessage()
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
			if _, ok := msg.(*session.ClientGoodbye); ok {
				close(clientDone)
				return
			}
		}
	}))
	defer hs.Close()

	addr := strings.TrimSuffix(strings.TrimPrefix("ws"+strings.TrimPrefix(hs.URL, "http"), "ws://"), "/")

	clientID, _ := secure.GenerateIdentity()
	recv, err := NewReceiverV2(V2Config{
		ServerAddr:    addr,
		Identity:      clientID,
		Name:          "v2-empty-client",
		MaxSampleRate: 48000,
		MaxBitDepth:   16,
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recv.Close()
	go func() {
		for range recv.Output() {
		}
	}()

	// The empty activate must not abort the connection.
	if err := recv.Connect(); err != nil {
		t.Fatalf("ReceiverV2.Connect with an empty activate: %v", err)
	}
	if !recv.connected.Load() {
		t.Error("client should remain connected after an empty activate")
	}
	recv.stateMu.Lock()
	avail := recv.available
	recv.stateMu.Unlock()
	if avail {
		t.Error("client should not report available before the player role activates")
	}
}

// TestReceiverV2_ClockSyncGraceExceedsBurstInterval guards against the
// pause/resume oscillation: clockSyncLoop bursts every 10s, so the clock-loss
// grace (and the ClockSync staleness window) must comfortably exceed that
// interval. With a grace shorter than the cadence the receiver syncs at t=0,
// the filter times out mid-cycle, playback pauses, then resumes at the next
// burst — pausing again forever. After a successful sync the receiver must
// still be fresh (not QualityLost) for most of a burst interval.
func TestReceiverV2_ClockSyncGraceExceedsBurstInterval(t *testing.T) {
	clientID, _ := secure.GenerateIdentity()

	recv, err := NewReceiverV2(V2Config{
		ServerAddr:    "127.0.0.1:1",
		Identity:      clientID,
		Name:          "v2-sync-grace-client",
		MaxSampleRate: 48000,
		MaxBitDepth:   16,
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recv.Close()

	// The default loss grace must sit well above the 10s clock-sync burst
	// cadence (clockSyncLoop) — otherwise the connection oscillates pause/resume.
	if recv.config.SyncLossGrace <= 15*time.Second {
		t.Fatalf("default SyncLossGrace = %v; must comfortably exceed the 10s clock-sync burst interval", recv.config.SyncLossGrace)
	}

	// A valid sync: offset 0, sub-ms rtt → QualityGood.
	cs := recv.ClockSync()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cs.SetClock(func() time.Time { return base })
	cs.ProcessSyncResponse(1_000_000, 1_000_050, 1_000_060, 1_000_110)

	// Advance to just under the grace: the filter must not have gone stale, so a
	// server that merely waits for the next burst is never declared lost.
	cs.SetClock(func() time.Time { return base.Add(recv.config.SyncLossGrace - time.Second) })
	if q := cs.CheckQuality(); q == syncc.QualityLost {
		t.Errorf("clock quality lost after %v without a new burst (grace %v too short vs 10s cadence); would oscillate pause/resume",
			recv.config.SyncLossGrace-time.Second, recv.config.SyncLossGrace)
	}
}

// TestReceiverV2_BurstYieldsWhenAnotherInFlight guards the clock-sync burst
// serialization: clockSyncLoop (periodic) and activatePlayer's syncUntilReady
// can race to run a burst, and two concurrent bursts into the shared
// timeSyncResp channel steal each other's replies (duplicated "sync sample
// timed out" logs, multi-second RTT). A burst must yield immediately when
// another is already in flight.
func TestReceiverV2_BurstYieldsWhenAnotherInFlight(t *testing.T) {
	clientID, _ := secure.GenerateIdentity()
	recv, err := NewReceiverV2(V2Config{
		ServerAddr:    "127.0.0.1:1",
		Identity:      clientID,
		Name:          "v2-burst-yield-client",
		MaxSampleRate: 48000,
		MaxBitDepth:   16,
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recv.Close()

	// Simulate another burst owner (e.g. clockSyncLoop) holding the lock, with
	// no live connection — runTimeSyncBurst must return immediately rather than
	// proceed (which without the lock would dereference the nil conn).
	recv.syncBurstMu.Lock()
	done := make(chan struct{})
	go func() {
		recv.runTimeSyncBurst(V2TimeSyncBurstSize)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runTimeSyncBurst did not yield while another burst held the lock")
	}
	recv.syncBurstMu.Unlock()
}

// TestReceiverV2_AdvertisesSupportedCommands guards the player state's
// supported_commands: the spec makes it required and says volume only applies
// when "volume" is advertised. A player that omits it reads as accepting no
// controls in HA (volume stuck at 0). The reported list must include every
// server→player verb this receiver handles (volume, mute, set_output_delay).
func TestReceiverV2_AdvertisesSupportedCommands(t *testing.T) {
	longTerm, _ := secure.NewPSK()
	store, err := NewFileStore(t.TempDir() + "/pairings.json")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := store.PutRecord(longTerm); err != nil {
		t.Fatal(err)
	}

	supported := make(chan []string, 1)
	serverID, _ := secure.GenerateIdentity()
	clock := func() int64 { return 0 }
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn, _, err := secure.ServerHandshake(ws, secure.ServerHandshakeConfig{
			Identity: serverID,
			SelectPSK: func(string) (secure.PSK, secure.PSKCategoryCode, error) {
				return longTerm, secure.CategoryLongTerm, nil
			},
		})
		if err != nil {
			return
		}
		if _, err := session.EstablishServer(conn, session.ServerConfig{
			ServerName:        "v2-test-peer",
			PSKCategory:       session.PSKLongTerm,
			InitialActivities: []session.Activity{session.ActivityPlayback},
			ChooseActiveRoles: func(session.ClientHello) []string { return []string{"player@v1"} },
		}); err != nil {
			return
		}
		for {
			msgType, payload, err := conn.ReadMessage()
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
				_ = writeControl(conn, session.ServerTime{
					ClientTransmitted: v.ClientTransmitted,
					ServerReceived:    clock(),
					ServerTransmitted: clock(),
				})
			case *session.ClientState:
				if v.Available && v.Player != nil {
					supported <- v.Player.SupportedCommands
					return
				}
			}
		}
	}))
	defer hs.Close()

	addr := strings.TrimSuffix(strings.TrimPrefix("ws"+strings.TrimPrefix(hs.URL, "http"), "ws://"), "/")
	clientID, _ := secure.GenerateIdentity()
	recv, err := NewReceiverV2(V2Config{
		ServerAddr:    addr,
		Identity:      clientID,
		Name:          "v2-cmd-client",
		Store:         store,
		MaxSampleRate: 48000,
		MaxBitDepth:   16,
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recv.Close()
	go func() {
		for range recv.Output() {
		}
	}()

	if err := recv.Connect(); err != nil {
		t.Fatalf("ReceiverV2.Connect: %v", err)
	}

	select {
	case cmds := <-supported:
		for _, want := range []string{"volume", "mute", "set_output_delay"} {
			if !slices.Contains(cmds, want) {
				t.Errorf("player supported_commands %v missing %q", cmds, want)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("peer never received an available client/state with a player object")
	}
}

// TestReceiverV2_LaterActivateSyncsClock exercises the path where the player
// role is granted by a later server/activate (as Home Assistant does after
// holding the connection empty), rather than by the initial activate. The
// later activation must run on its own goroutine: activatePlayer performs the
// initial clock-sync burst, which needs the readLoop to keep draining
// server/time replies. Running it inline on the readLoop goroutine starves the
// burst (the only reader is busy running it), so every probe times out and the
// filter stays unsynced until the far-off periodic burst. This asserts the
// filter converges promptly after activation.
func TestReceiverV2_LaterActivateSyncsClock(t *testing.T) {
	longTerm, _ := secure.NewPSK()
	store, err := NewFileStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRecord(longTerm); err != nil {
		t.Fatal(err)
	}

	available := make(chan struct{}, 1)
	serverID, _ := secure.GenerateIdentity()
	start := time.Now()
	clock := func() int64 { return time.Since(start).Microseconds() }
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn, _, err := secure.ServerHandshake(ws, secure.ServerHandshakeConfig{
			Identity: serverID,
			SelectPSK: func(string) (secure.PSK, secure.PSKCategoryCode, error) {
				return longTerm, secure.CategoryLongTerm, nil
			},
		})
		if err != nil {
			return
		}
		if _, err := session.EstablishServer(conn, session.ServerConfig{
			ServerName:        "v2-later-peer",
			PSKCategory:       session.PSKLongTerm,
			InitialActivities: []session.Activity{session.ActivityPlayback},
			ChooseActiveRoles: func(session.ClientHello) []string { return []string{} },
		}); err != nil {
			return
		}
		// After the client's Connect has returned (empty activate → readLoop
		// running), drive a later server/activate that grants the player role.
		go func() {
			time.Sleep(250 * time.Millisecond)
			roles := []string{"player@v1"}
			_ = writeControl(conn, session.ServerActivate{
				Activities:  []session.Activity{session.ActivityPlayback},
				ActiveRoles: &roles,
			})
		}()
		for {
			msgType, payload, err := conn.ReadMessage()
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
				_ = writeControl(conn, session.ServerTime{
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
	recv, err := NewReceiverV2(V2Config{
		ServerAddr:    addr,
		Identity:      clientID,
		Name:          "v2-later-client",
		Store:         store,
		MaxSampleRate: 48000,
		MaxBitDepth:   16,
	})
	if err != nil {
		t.Fatalf("NewReceiverV2: %v", err)
	}
	defer recv.Close()
	go func() {
		for range recv.Output() {
		}
	}()
	if err := recv.Connect(); err != nil {
		t.Fatalf("ReceiverV2.Connect: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-available:
			// Availability means syncUntilReady produced its first sample, so
			// the filter must have converged.
			if recv.ClockSync().Synced() {
				return
			}
		case <-deadline:
			t.Fatal("clock never converged after a later role activation; the initial burst starved (activation must run off the readLoop goroutine)")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
