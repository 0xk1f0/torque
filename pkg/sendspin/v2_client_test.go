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
	"github.com/gorilla/websocket"
)

// v2TestPeer is a minimal encrypted Sendspin server built purely so the client
// has someone to talk to: Noise server handshake, session establishment, and a
// control loop that answers clock sync and, once the client reports available,
// streams a short PCM tone. It exists only for tests; there is no server
// product in this repository.
type v2TestPeer struct {
	hs *httptest.Server
}

// newV2TestPeer starts the peer. onAvailable receives true the first time the
// client reports available:true.
func newV2TestPeer(t *testing.T, onAvailable func()) *v2TestPeer {
	t.Helper()
	serverID, err := secure.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	upgrader := websocket.Upgrader{}
	start := time.Now()
	clock := func() int64 { return time.Since(start).Microseconds() }

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Origin"), "http") {
			t.Errorf("server accepted an Origin header; spec forbids it")
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handlePeerConn(t, ws, serverID, clock, onAvailable)
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

func handlePeerConn(t *testing.T, ws *websocket.Conn, serverID *secure.Identity, clock func() int64, onAvailable func()) {
	conn, _, err := secure.ServerHandshake(ws, secure.ServerHandshakeConfig{Identity: serverID})
	if err != nil {
		return
	}
	sess, err := session.EstablishServer(conn, session.ServerConfig{
		ServerName:        "v2-test-peer",
		PSKCategory:       session.PSKSentinel,
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
	peer := newV2TestPeer(t, nil)
	addr := peerAddr(peer)

	clientID, _ := secure.GenerateIdentity()
	recv, err := NewReceiverV2(V2Config{
		ServerAddr:     addr,
		Identity:       clientID,
		Name:           "v2-loopback-client",
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
	peer := newV2TestPeer(t, func() { available <- struct{}{} })
	addr := peerAddr(peer)

	clientID, _ := secure.GenerateIdentity()
	recv, _ := NewReceiverV2(V2Config{
		ServerAddr:    addr,
		Identity:      clientID,
		Name:          "v2-sync-client",
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
	peer := newV2TestPeer(t, nil)
	addr := peerAddr(peer)

	clientID, _ := secure.GenerateIdentity()
	p, err := NewPlayerV2(V2Config{
		ServerAddr:    addr,
		Identity:      clientID,
		Name:          "v2-player",
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
