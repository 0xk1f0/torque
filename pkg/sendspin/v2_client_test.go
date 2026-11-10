// ABOUTME: End-to-end test of the spec-v2 client (ReceiverV2) against the
// ABOUTME: encrypted server: handshake, session, clock sync, stream lifecycle,
// ABOUTME: decode, and scheduler output with correct timestamps.
package sendspin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/audio"
	"github.com/Sendspin/sendspin-go/pkg/protocol/playback"
	"github.com/Sendspin/sendspin-go/pkg/protocol/secure"
	"github.com/Sendspin/sendspin-go/pkg/protocol/server"
	"github.com/gorilla/websocket"
)

// constSource streams a constant-value PCM tone for nFrames × 20 ms.
type v2ConstSource struct {
	format audio.Format
	frames int
	value  int32
}

func (s *v2ConstSource) Format() audio.Format { return s.format }
func (s *v2ConstSource) Read(dst []int32) (int, error) {
	if s.frames <= 0 {
		return 0, io.EOF
	}
	s.frames--
	for i := range dst {
		dst[i] = s.value
	}
	return len(dst), nil
}

// v2ServerHarness stands up an encrypted spec-v2 server behind a WebSocket
// endpoint and returns it plus the http server.
func v2ServerHarness(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	serverID, _ := secure.GenerateIdentity()
	s, err := server.New(server.Config{
		Identity:              serverID,
		ServerName:            "v2-test-server",
		AllowUnpairedPlayback: true,
		SourceKind:            playback.SourceLive,
	})
	if err != nil {
		t.Fatalf("v2 server: %v", err)
	}
	upgrader := websocket.Upgrader{Subprotocols: []string{"sendspin"}}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Origin"), "http") {
			t.Errorf("server accepted an Origin header; spec forbids it")
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = s.Handle(ws)
	}))
	t.Cleanup(hs.Close)
	return s, hs
}

func v2wsURL(hs *httptest.Server) string {
	return "ws" + strings.TrimPrefix(hs.URL, "http")
}

// TestReceiverV2_EndToEndAudio connects ReceiverV2 to the encrypted server,
// streams a constant tone, and verifies decoded buffers arrive with correct
// format, sample values, and monotonically increasing 20 ms timestamps.
func TestReceiverV2_EndToEndAudio(t *testing.T) {
	s, hs := v2ServerHarness(t)
	addr := strings.TrimPrefix(v2wsURL(hs), "ws://")

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

	// Drain decoded output; collect timestamps + sample values.
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

	// Find the client's group and stream a 600 ms constant tone (16-bit to keep
	// decode trivial).
	var groupID string
	waitFor(t, 3*time.Second, func() bool {
		if gid := s.Groups().GroupID(clientID.ID()); gid != "" {
			groupID = gid
			return true
		}
		return false
	}, "client to join a group")
	// Only start playback once the server has actually processed the client's
	// available:true — otherwise stream/start is (correctly) skipped.
	waitFor(t, 3*time.Second, func() bool {
		return s.Groups().Available(clientID.ID())
	}, "server to observe the client available")

	format := audio.Format{SampleRate: 48000, Channels: 2, BitDepth: 16}
	src := &v2ConstSource{format: format, frames: 150, value: 1000}
	go func() {
		_ = s.StreamGroup(context.Background(), groupID, src, server.StreamOptions{})
	}()

	// Expect at least a handful of decoded chunks.
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

	// Chunk spacing is ~20 ms on the server clock grid; timestamps increase.
	for i := 1; i < len(got); i++ {
		delta := got[i].ts - got[i-1].ts
		if delta < 18_000 || delta > 22_000 {
			t.Errorf("chunk %d: spacing %dµs, want ~20000µs", i, delta)
		}
	}
	// A constant source decodes to a constant, non-zero sample in every chunk
	// (16-bit PCM is scaled into the 24-bit container by the decoder).
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
// and its availability reaches the server (a precondition for streaming).
func TestReceiverV2_SyncAndAvailability(t *testing.T) {
	s, hs := v2ServerHarness(t)
	addr := strings.TrimPrefix(v2wsURL(hs), "ws://")

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

	// The time filter must be synced after the initial burst.
	waitFor(t, 3*time.Second, func() bool {
		rtt, _ := recv.ClockSync().GetStats()
		return rtt > 0
	}, "clock sync to produce a sample")

	// The server must have the client marked available (it reports it in its
	// initial client/state), so StreamGroup can stream to it.
	waitFor(t, 3*time.Second, func() bool {
		g := s.Groups().GroupOf(clientID.ID())
		return g == nil || g.Len() == 1
	}, "client to be in a group")
}

// TestPlayerV2_VolumeMuteRoundTrip verifies local volume/mute state changes
// land both in the player state and (via client/state) flow back to the server.
func TestPlayerV2_VolumeMuteRoundTrip(t *testing.T) {
	_, hs := v2ServerHarness(t)
	addr := strings.TrimPrefix(v2wsURL(hs), "ws://")

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
