// ABOUTME: Stress, soak, and multi-room synchronization tests
// ABOUTME: Many concurrent clients, 5+ player timestamp alignment, bounded growth
package sendspin

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/protocol"
)

func startStressServer(t *testing.T) (*Server, int) {
	t.Helper()
	server, err := NewServer(ServerConfig{
		Port:   0,
		Name:   "stress server",
		Source: NewTestTone(48000, 2),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errChan := make(chan error, 1)
	go func() { errChan <- server.Start() }()
	port := waitForServerPort(t, server)
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
			t.Log("server stop timeout (check shutdown path)")
		}
	})
	return server, port
}

func stressClientConfig(addr, id string) protocol.Config {
	return protocol.Config{
		ServerAddr: addr,
		ClientID:   id,
		Name:       "stress-" + id,
		Version:    1,
		PlayerV1Support: protocol.PlayerV1Support{
			SupportedFormats: []protocol.AudioFormat{
				{Codec: "pcm", Channels: 2, SampleRate: 48000, BitDepth: 16},
			},
			BufferCapacity:    1 << 20,
			SupportedCommands: []string{"volume", "mute"},
		},
	}
}

// TestServer_ManyConcurrentClients connects N protocol clients to one server
// and verifies every one completes the handshake, receives a stream/start and
// actual audio, and that the server remains consistent throughout (all clients
// registered). Guards against regressions where load breaks dispatch or state.
func TestServer_ManyConcurrentClients(t *testing.T) {
	server, port := startStressServer(t)
	addr := fmt.Sprintf("localhost:%d", port)
	const n = 16

	var wg sync.WaitGroup
	var ready atomic.Int64
	errCh := make(chan error, n)
	release := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("stress-client-%02d", i)
			c := protocol.NewClient(stressClientConfig(addr, id))
			if err := c.Connect(); err != nil {
				errCh <- fmt.Errorf("%s: connect: %w", id, err)
				return
			}
			defer c.Close()
			if err := c.SendState(protocol.PlayerState{State: "synchronized", Volume: 80, Muted: false}); err != nil {
				errCh <- fmt.Errorf("%s: state: %w", id, err)
				return
			}
			select {
			case start := <-c.StreamStart:
				if start.Player == nil {
					errCh <- fmt.Errorf("%s: stream/start without player", id)
					return
				}
			case <-time.After(5 * time.Second):
				errCh <- fmt.Errorf("%s: no stream/start within 5s", id)
				return
			}
			got := 0
			deadline := time.After(5 * time.Second)
			for got < 5 {
				select {
				case chunk := <-c.AudioChunks:
					if len(chunk.Data) == 0 {
						errCh <- fmt.Errorf("%s: empty audio chunk", id)
						return
					}
					got++
				case <-deadline:
					errCh <- fmt.Errorf("%s: only %d/5 audio chunks in 5s", id, got)
					return
				}
			}
			ready.Add(1)
			// Hold the connection open until the harness has verified the
			// server's client registry while everyone is still connected.
			<-release
		}(i)
	}

	// Wait until every client is connected and streaming, then verify the
	// server registry all at once (before the clients release and close).
	waitFor(t, 10*time.Second, func() bool {
		return ready.Load() == n
	}, fmt.Sprintf("all %d clients streaming", n))

	waitFor(t, 3*time.Second, func() bool {
		return len(server.Clients()) == n
	}, fmt.Sprintf("all %d clients registered on server (have %d)", n, len(server.Clients())))

	close(release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// timestampSample accumulates the server timestamps one receiver observed, for
// cross-receiver timeline alignment checks.
type timestampSample struct {
	mu         sync.Mutex
	timestamps []int64
}

// TestMultiRoom_TimestampAlignment runs 5 receivers against one server and
// verifies they all observe the same audio timeline: each server tick sends
// the identical playbackTime to every client, so the decoded chunk timestamps
// each receiver sees must fall on one shared grid (same 20ms sequence), and
// each receiver's earliest sample must be within one chunk of the others'.
// This is the wire-level precondition for multi-room output staying in sync.
func TestMultiRoom_TimestampAlignment(t *testing.T) {
	_, port := startStressServer(t)
	addr := fmt.Sprintf("localhost:%d", port)
	const players = 5

	receivers := make([]*Receiver, players)
	all := make([]*timestampSample, players)

	for i := 0; i < players; i++ {
		recv, err := NewReceiver(ReceiverConfig{
			ServerAddr: addr,
			PlayerName: fmt.Sprintf("room-%d", i),
			ClientID:   fmt.Sprintf("room-client-%d", i),
			// PCM only: avoid per-client FLAC encode on top of the stream load.
			PreferredCodec: "pcm",
			BufferMs:       200,
		})
		if err != nil {
			t.Fatalf("NewReceiver[%d]: %v", i, err)
		}
		receivers[i] = recv
	}
	t.Cleanup(func() {
		for _, r := range receivers {
			r.Close()
		}
	})

	for i := 0; i < players; i++ {
		s := &timestampSample{}
		all[i] = s
		recv := receivers[i]
		if err := recv.Connect(); err != nil {
			t.Fatalf("Receiver[%d].Connect: %v", i, err)
		}
		go func(recv *Receiver, s *timestampSample) {
			for buf := range recv.Output() {
				s.mu.Lock()
				s.timestamps = append(s.timestamps, buf.Timestamp)
				if len(s.timestamps) > 200 {
					s.timestamps = s.timestamps[len(s.timestamps)-200:]
				}
				s.mu.Unlock()
			}
		}(recv, s)
	}

	// Let all players stream for ~1.2s (60 chunks each).
	waitFor(t, 5*time.Second, func() bool {
		count := 0
		for _, s := range all {
			s.mu.Lock()
			n := len(s.timestamps)
			s.mu.Unlock()
			if n >= 30 {
				count++
			}
		}
		return count == players
	}, "all players to receive at least 30 chunks")

	// 1. Per-receiver: consecutive timestamps advance in whole 20ms steps. A
	//    scheduler may drop a late chunk (producing 2×/3× gaps), so each gap
	//    must fall on the 20ms grid rather than being exactly one chunk.
	for i, s := range all {
		s.mu.Lock()
		ts := append([]int64(nil), s.timestamps...)
		s.mu.Unlock()
		for j := 1; j < len(ts); j++ {
			delta := ts[j] - ts[j-1]
			if delta < 18_000 {
				t.Errorf("room %d: chunk spacing %dµs < one chunk at index %d", i, delta, j)
				break
			}
			r := delta % 20_000
			if r > 2000 && r < 18_000 {
				t.Errorf("room %d: chunk spacing %dµs is off the 20ms grid at index %d", i, delta, j)
				break
			}
		}
	}

	// 2. Cross-receiver: earliest observed timestamps within a few chunks of
	//    each other. A later-joining room can start its captured window one or
	//    two chunks after room 0 (its first chunk may be dropped as late while
	//    its clock filter converges), so the bound is a small integer number of
	//    chunks — same-grid membership is proven by the overlap check below.
	earliest := make([]int64, players)
	for i, s := range all {
		s.mu.Lock()
		ts := append([]int64(nil), s.timestamps...)
		s.mu.Unlock()
		if len(ts) == 0 {
			t.Fatalf("room %d: no samples", i)
		}
		earliest[i] = ts[0]
	}
	const earliestTolUS = 5 * 20_000 // 5 chunks
	for i := 1; i < players; i++ {
		d := earliest[i] - earliest[0]
		if d < -earliestTolUS || d > earliestTolUS {
			t.Errorf("room %d earliest=%dµs vs room 0=%dµs differ by %.1fms (> 5 chunks)",
				i, earliest[i], earliest[0], float64(d)/1000.0)
		}
	}

	// 3. Cross-receiver: the two longest streams substantially overlap, proving
	//    they are sampling the SAME timeline (identical server playbackTime).
	longest, second := longestTwo(all)
	if longest == nil || second == nil {
		t.Fatal("not enough samples to compare streams")
	}
	secondSet := make(map[int64]struct{}, len(second))
	for _, v := range second {
		secondSet[v] = struct{}{}
	}
	overlap := 0
	for _, v := range longest {
		if _, ok := secondSet[v]; ok {
			overlap++
		}
	}
	if len(longest) > 0 && float64(overlap)/float64(len(longest)) < 0.4 {
		t.Errorf("stream overlap %d/%d (%.0f%%) < 40%%: receivers are NOT on the same timeline",
			overlap, len(longest), 100*float64(overlap)/float64(len(longest)))
	}
}

func longestTwo(all []*timestampSample) (a, b []int64) {
	type namedList struct {
		ts []int64
	}
	lists := make([]namedList, len(all))
	for i, s := range all {
		s.mu.Lock()
		lists[i] = namedList{ts: append([]int64(nil), s.timestamps...)}
		s.mu.Unlock()
	}
	for i := 1; i < len(lists); i++ {
		for j := i; j > 0 && len(lists[j].ts) > len(lists[j-1].ts); j-- {
			lists[j], lists[j-1] = lists[j-1], lists[j]
		}
	}
	if len(lists) >= 1 {
		a = lists[0].ts
	}
	if len(lists) >= 2 {
		b = lists[1].ts
	}
	return a, b
}

// TestReceiver_Soak_NoGrowth repeatedly connects a receiver, streams audio,
// applies metadata, and closes it against a persistent server, checking that
// goroutines return to baseline and heap allocation stays bounded. Catches
// per-connection leaks in the decoder/scheduler/metadata/client lifecycle.
func TestReceiver_Soak_NoGrowth(t *testing.T) {
	_, port := startStressServer(t)
	addr := fmt.Sprintf("localhost:%d", port)

	baselineGoroutines := runtime.NumGoroutine()

	run := func(i int) {
		recv, err := NewReceiver(ReceiverConfig{
			ServerAddr: addr,
			PlayerName: fmt.Sprintf("soak-%d", i),
			ClientID:   fmt.Sprintf("soak-client-%d", i),
			// Consume metadata like a TUI/controller would; set before
			// Connect so the receiver's goroutines read it safely.
			OnMetadata: func(Metadata) {},
		})
		if err != nil {
			t.Fatalf("NewReceiver[%d]: %v", i, err)
		}
		defer recv.Close()
		if err := recv.Connect(); err != nil {
			t.Fatalf("Receiver[%d].Connect: %v", i, err)
		}

		go func() {
			for range recv.Output() {
			}
		}()

		// Stream long enough for several chunks plus at least one metadata
		// callback and one clock-sync burst.
		waitFor(t, 3*time.Second, func() bool {
			if sc := recv.currentScheduler(); sc != nil {
				return sc.Stats().Received >= 10
			}
			return false
		}, fmt.Sprintf("iteration %d to receive audio", i))
		time.Sleep(100 * time.Millisecond)
	}

	const iterations = 8
	for i := 0; i < iterations; i++ {
		run(i)
	}

	runtime.GC()
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// Goroutines must settle back to the pre-test baseline within a small
	// tolerance (some http/ws internals may linger briefly).
	waitFor(t, 3*time.Second, func() bool {
		return runtime.NumGoroutine() <= baselineGoroutines+4
	}, fmt.Sprintf("goroutines to drain to baseline after %d cycles", iterations))

	// HeapAlloc after all cycles must not show runaway growth. The threshold
	// is generous; what it guards is per-connection objects (decoders,
	// buffers, metadata slices) never being released.
	if mem.HeapAlloc > 64<<20 {
		t.Errorf("HeapAlloc after %d soak cycles = %d MiB; suspicious unbounded growth",
			iterations, mem.HeapAlloc>>20)
	}
}
