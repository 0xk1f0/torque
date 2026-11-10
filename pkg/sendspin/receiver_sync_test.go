// ABOUTME: Tests for graceful degradation on clock-sync loss
// ABOUTME: Covers the output gate and the sync-loss monitor state machine
package sendspin

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sendspin/sendspin-go/pkg/audio"
	sendspinsync "github.com/Sendspin/sendspin-go/pkg/sync"
)

// fakeClock is a manually-advanced clock used to drive ClockSync and the
// sync-loss monitor deterministically without waiting on real time.
type fakeClock struct {
	t atomic.Int64 // time.Time.UnixNano, atomically updated
}

func newFakeClock(t time.Time) *fakeClock {
	f := &fakeClock{}
	f.t.Store(t.UnixNano())
	return f
}

func (f *fakeClock) Now() time.Time {
	return time.Unix(0, f.t.Load())
}

func (f *fakeClock) Set(t time.Time) { f.t.Store(t.UnixNano()) }

func (f *fakeClock) Advance(d time.Duration) {
	f.t.Add(int64(d)) // atomic.AddInt64 via atomic.Int64
}

// seedSync records a perfect (zero-RTT) sync response at the fake clock's
// current time, leaving the filter synced with offset ~0 and quality Good.
func seedSync(cs *sendspinsync.ClockSync, fake *fakeClock) {
	nowUs := fake.Now().UnixMicro()
	cs.ProcessSyncResponse(nowUs, nowUs+500, nowUs+1000, nowUs+1500)
	// Realign the fake wall clock with the latest measurement time so
	// TimeSinceLastSync sees a consistent baseline.
	fake.Set(time.UnixMicro(nowUs + 1500))
}

// freshSync records a new sync response advancing the fake clock by d so the
// filter is refreshed (quality returns to Good after a loss).
func freshSync(cs *sendspinsync.ClockSync, fake *fakeClock, d time.Duration) {
	fake.Advance(d)
	nowUs := fake.Now().UnixMicro()
	cs.ProcessSyncResponse(nowUs, nowUs+500, nowUs+1000, nowUs+1500)
	fake.Set(time.UnixMicro(nowUs + 1500))
}

// TestReceiver_SyncLossMonitor exercises the full monitor state machine with a
// fake clock: an unsynced receiver must not pause, a sustained QualityLost
// after an established baseline must pause + report ErrSyncLost, and a fresh
// sync response must resume + report ErrSyncRecovered.
func TestReceiver_SyncLossMonitor(t *testing.T) {
	recv, err := NewReceiver(ReceiverConfig{
		ServerAddr:          "localhost:1", // never dialed
		PlayerName:          "sync-loss test",
		SyncMonitorInterval: 10 * time.Millisecond,
		SyncLossGrace:       120 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	defer recv.Close()

	fake := newFakeClock(time.UnixMicro(1_000_000))
	cs := recv.clockSync
	cs.SetClock(fake.Now)
	cs.SetStaleAfter(80 * time.Millisecond)

	errCh := make(chan error, 16)
	recv.config.OnError = func(e error) { errCh <- e }

	go recv.watchClockSync()

	// 1. Before any sync response the filter is unsynced: quality is QualityLost
	//    but the monitor must NOT pause (startup is handled by the scheduler).
	time.Sleep(60 * time.Millisecond)
	if recv.syncLost.Load() {
		t.Fatal("monitor paused playback before an established sync baseline")
	}

	// 2. Establish sync, then let it go stale for longer than the grace period.
	seedSync(cs, fake)
	fake.Advance(300 * time.Millisecond)

	waitFor(t, 2*time.Second, func() bool {
		return recv.syncLost.Load()
	}, "playback to pause after sustained clock-sync loss")
	if !hasErrFrom(errCh, ErrSyncLost) {
		t.Errorf("expected ErrSyncLost reported")
	}

	// 3. A fresh sync response revives the filter; playback must resume.
	freshSync(cs, fake, 1*time.Millisecond)
	waitFor(t, 2*time.Second, func() bool {
		return !recv.syncLost.Load()
	}, "playback to resume after clock-sync recovery")
	if !hasErrFrom(errCh, ErrSyncRecovered) {
		t.Errorf("expected ErrSyncRecovered reported")
	}
}

// TestReceiver_SyncLossOutputGate verifies that while syncLost is set,
// pumpSchedulerOutput discards ready buffers instead of forwarding them, and
// that clearing the gate resumes forwarding. A permanent drainer consumes the
// output so sends never block and the counter tells exactly what was forwarded.
func TestReceiver_SyncLossOutputGate(t *testing.T) {
	recv, err := NewReceiver(ReceiverConfig{
		ServerAddr: "localhost:1", // never dialed
		PlayerName: "sync-loss gate test",
	})
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	defer recv.Close()

	// Sync the filter with a zero-RTT response so server timestamps map to
	// ~now locally and scheduled buffers become playable immediately.
	nowUs := time.Now().UnixMicro()
	recv.clockSync.ProcessSyncResponse(nowUs, nowUs+500, nowUs+1000, nowUs+1500)

	ctx, cancel := context.WithCancel(recv.ctx)
	defer cancel()

	sched := NewScheduler(recv.clockSync, 500, 0)
	go sched.Run()
	defer sched.Stop()

	// Register the pump with recv.pumpWG just as handleStreamStart would, so
	// recv.Close (deferred below) waits for it before closing r.output. The
	// pump's own defer handles the Done.
	recv.pumpWG.Add(1)
	go recv.pumpSchedulerOutput(ctx, sched)

	var forwarded atomic.Int64
	go func() {
		for range recv.output {
			forwarded.Add(1)
		}
	}()

	feed := func(n int) {
		for i := 0; i < n; i++ {
			buf := audio.Buffer{
				Timestamp: recv.clockSync.ServerMicrosNow(),
				Samples:   make([]int32, 960),
				Format:    audio.Format{Codec: "pcm", SampleRate: 48000, Channels: 2, BitDepth: 16},
			}
			sched.Schedule(buf)
			time.Sleep(2 * time.Millisecond)
		}
	}

	// With the gate open the pipeline must deliver. Feed more than the
	// startup buffer target (500ms / 20ms = 25 chunks) so the scheduler
	// actually starts emitting.
	feed(40)
	waitFor(t, 2*time.Second, func() bool {
		return forwarded.Load() > 0
	}, "buffers to be forwarded with the sync gate open")
	before := forwarded.Load()

	// Close the gate: freshly scheduled buffers must NOT reach the output.
	recv.syncLost.Store(true)
	feed(20)
	time.Sleep(200 * time.Millisecond)
	if got := forwarded.Load(); got != before {
		t.Fatalf("gate closed but %d buffers were forwarded (want %d unchanged)", got-before, before)
	}

	// Open the gate again: delivery resumes.
	recv.syncLost.Store(false)
	feed(40)
	waitFor(t, 2*time.Second, func() bool {
		return forwarded.Load() > before
	}, "buffers to be forwarded again once the sync gate reopens")
}

// hasErrFrom reports whether target has been observed on errCh, draining it.
func hasErrFrom(errCh chan error, target error) bool {
	for {
		select {
		case e := <-errCh:
			if errors.Is(e, target) {
				return true
			}
		default:
			return false
		}
	}
}
