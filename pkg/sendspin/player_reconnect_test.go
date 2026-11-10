// ABOUTME: Tests for reconnect handling and automatic retry
// ABOUTME: Covers backoff give-up, rediscover success, end-to-end reconnection,
// ABOUTME: and goroutine hygiene across connection drops
package sendspin

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// fakeOutput is a minimal output.Output that never touches miniaudio, so
// building a Player/Receiver in tests skips the device capability probe.
type fakeOutput struct{}

func (fakeOutput) Open(sampleRate, channels, bitDepth int) error { return nil }
func (fakeOutput) Write(samples []int32) error                   { return nil }
func (fakeOutput) SetVolume(v int)                               {}
func (fakeOutput) SetMuted(m bool)                               {}
func (fakeOutput) Close() error                                  { return nil }

func testPlayerConfig(addr string, reconnect ReconnectConfig) PlayerConfig {
	return PlayerConfig{
		ServerAddr:    addr,
		PlayerName:    "reconnect-test",
		ClientID:      "reconnect-test-client",
		Output:        fakeOutput{},
		MaxSampleRate: 48000, // explicit cap: no device probe in tests
		MaxBitDepth:   16,
		Reconnect:     reconnect,
	}
}

// TestPlayer_ReconnectWithBackoff_GivesUp verifies the failure path: after
// MaxAttempts failed dials the loop aborts and reports, without hanging.
func TestPlayer_ReconnectWithBackoff_GivesUp(t *testing.T) {
	p, err := NewPlayer(testPlayerConfig("localhost:1", ReconnectConfig{
		Enabled:      true,
		InitialDelay: 5 * time.Millisecond,
		MaxDelay:     20 * time.Millisecond,
		Multiplier:   2.0,
		MaxAttempts:  3,
	}))
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}
	defer p.Close()

	start := time.Now()
	recv, ok := p.reconnectWithBackoff()
	if ok {
		recv.Close()
		t.Fatal("expected reconnect to give up against an unreachable server")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("reconnectWithBackoff took %v with 3x20ms backoff; too slow", elapsed)
	}
}

// TestPlayer_ReconnectWithBackoff_RediscoverConnects verifies the success
// path: a Rediscover callback that resolves a reachable address lets the loop
// land a working receiver.
func TestPlayer_ReconnectWithBackoff_RediscoverConnects(t *testing.T) {
	server, err := NewServer(ServerConfig{
		Port:   0,
		Name:   "rediscover server",
		Source: NewTestTone(48000, 2),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errChan := make(chan error, 1)
	go func() { errChan <- server.Start() }()
	defer func() {
		server.Stop()
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
			t.Error("server stop timeout")
		}
	}()
	port := waitForServerPort(t, server)
	addr := fmt.Sprintf("localhost:%d", port)

	var rediscoverCalls int
	p, err := NewPlayer(testPlayerConfig(addr, ReconnectConfig{
		Enabled:      true,
		InitialDelay: 5 * time.Millisecond,
		MaxDelay:     20 * time.Millisecond,
		Multiplier:   2.0,
		MaxAttempts:  3,
		Rediscover: func(ctx context.Context) (string, error) {
			rediscoverCalls++
			return addr, nil
		},
	}))
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}
	defer p.Close()

	recv, ok := p.reconnectWithBackoff()
	if !ok {
		t.Fatalf("expected reconnect to succeed via rediscover, got failure")
	}
	defer recv.Close()

	if rediscoverCalls == 0 {
		t.Error("expected Rediscover to be consulted before the attempt")
	}
}

// TestPlayer_ReconnectLoop_ReconnectsAfterServerRestart runs the full
// reconnect loop: connect to server A, kill it, bring up server B on a new
// address (surfaced through Rediscover), and assert the player transitions
// through reconnecting back to connected on B.
func TestPlayer_ReconnectLoop_ReconnectsAfterServerRestart(t *testing.T) {
	var mu sync.Mutex
	var currentAddr string

	// startTestServer binds an ephemeral port, publishes its address into
	// currentAddr, and registers an idempotent stop for cleanup.
	type managedServer struct {
		srv  *Server
		err  chan error
		once sync.Once
	}
	starts := make([]*managedServer, 0, 2)
	startTestServer := func(name string) *managedServer {
		srv, err := NewServer(ServerConfig{
			Port:   0,
			Name:   name,
			Source: NewTestTone(48000, 2),
		})
		if err != nil {
			t.Fatalf("NewServer(%s): %v", name, err)
		}
		ms := &managedServer{srv: srv, err: make(chan error, 1)}
		starts = append(starts, ms)
		go func() { ms.err <- srv.Start() }()
		port := waitForServerPort(t, srv)
		mu.Lock()
		currentAddr = fmt.Sprintf("localhost:%d", port)
		mu.Unlock()
		return ms
	}
	stopServer := func(ms *managedServer) {
		ms.once.Do(func() {
			ms.srv.Stop()
			select {
			case <-ms.err:
			case <-time.After(5 * time.Second):
			}
		})
	}
	t.Cleanup(func() {
		for _, ms := range starts {
			stopServer(ms)
		}
	})

	serverA := startTestServer("server a")
	addrA := func() string {
		mu.Lock()
		defer mu.Unlock()
		return currentAddr
	}()

	var states []PlayerState
	var statesMu sync.Mutex
	recordState := func(s PlayerState) {
		statesMu.Lock()
		states = append(states, s)
		statesMu.Unlock()
	}

	p, err := NewPlayer(PlayerConfig{
		ServerAddr:    addrA,
		PlayerName:    "reconnect-loop-test",
		ClientID:      "reconnect-loop-client",
		Output:        fakeOutput{},
		MaxSampleRate: 48000,
		MaxBitDepth:   16,
		OnStateChange: recordState,
		Reconnect: ReconnectConfig{
			Enabled:      true,
			InitialDelay: 30 * time.Millisecond,
			MaxDelay:     200 * time.Millisecond,
			Multiplier:   2.0,
			MaxAttempts:  0, // infinite
			Rediscover: func(_ context.Context) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				return currentAddr, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}
	defer p.Close()

	if err := p.Connect(); err != nil {
		t.Fatalf("Player.Connect: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool { return p.Status().Connected }, "player connected to server A")

	// Kill server A (Stop closes active connections, tripping the client read
	// loop promptly and firing the receiver's Done channel).
	stopServer(serverA)

	waitFor(t, 5*time.Second, func() bool {
		return !p.Status().Connected
	}, "player to notice the lost connection")

	// Give the loop a few failed retries against the dead address so the
	// reconnecting state is observable.
	time.Sleep(150 * time.Millisecond)

	// Bring up server B; Rediscover surfaces its address on the next attempt.
	startTestServer("server b")

	waitFor(t, 10*time.Second, func() bool {
		return p.Status().Connected
	}, "player to reconnect to server B")

	var sawReconnecting bool
	statesMu.Lock()
	for _, s := range states {
		if s.State == "reconnecting" {
			sawReconnecting = true
		}
	}
	statesMu.Unlock()
	if !sawReconnecting {
		t.Errorf("expected a 'reconnecting' state transition, states=%v", states)
	}
}

// TestReceiver_NoGoroutineLeakOnDrop verifies that dropping the server
// connection tears down every receiver-owned goroutine (scheduler Run, pump,
// decoder, metadata/sync monitors, client read/ping loops). Regression test
// for a leak where the scheduler's Run loop survived the receiver's death.
func TestReceiver_NoGoroutineLeakOnDrop(t *testing.T) {
	baseline := runtime.NumGoroutine()

	server, err := NewServer(ServerConfig{
		Port:   0,
		Name:   "leak test server",
		Source: NewTestTone(48000, 2),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errChan := make(chan error, 1)
	go func() { errChan <- server.Start() }()
	port := waitForServerPort(t, server)

	recv, err := NewReceiver(ReceiverConfig{
		ServerAddr: fmt.Sprintf("localhost:%d", port),
		PlayerName: "leak-test-receiver",
		ClientID:   "leak-test-client",
	})
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	if err := recv.Connect(); err != nil {
		t.Fatalf("Receiver.Connect: %v", err)
	}

	// Drain output so streaming proceeds normally.
	go func() {
		for range recv.Output() {
		}
	}()

	// Wait until the server is actually streaming to this receiver (a stream
	// start creates the scheduler whose Run goroutine could leak).
	waitFor(t, 3*time.Second, func() bool {
		c := recv.currentScheduler()
		return c != nil && c.Stats().Received >= 2
	}, "receiver to start receiving audio")

	// Kill the server; the receiver must wind down on its own.
	server.Stop()
	select {
	case <-errChan:
	case <-time.After(5 * time.Second):
		t.Error("server stop timeout")
	}

	waitFor(t, 3*time.Second, func() bool {
		select {
		case <-recv.Done():
			return true
		default:
			return false
		}
	}, "receiver to shut down after the drop")

	recv.Close() // release the client + log a clean close; idempotent

	waitFor(t, 3*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline+3
	}, fmt.Sprintf("goroutine count to return to baseline (want ≤ %d, have %d)",
		baseline, runtime.NumGoroutine()))
}
