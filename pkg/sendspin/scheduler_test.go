// ABOUTME: Tests for the playback scheduler
// ABOUTME: Covers static-delay offset, buffer ordering, and drop semantics
package sendspin

import (
	"testing"
	"time"

	"github.com/0xk1f0/torque/pkg/audio"
	"github.com/0xk1f0/torque/pkg/sync"
)

// TestScheduler_StaticDelayShift is a regression guard for the
// --static-delay-ms feature. With an unsynced ClockSync,
// ServerToLocalTime returns time.Unix(0, serverTime*1000) deterministically,
// which gives us an exact expected PlayAt we can check.
//
// For a 250ms static delay, a buffer whose timestamp would otherwise play
// at local time T should actually play at T + 250ms.
func TestScheduler_StaticDelayShift(t *testing.T) {
	const serverTimestampUs = int64(1_000_000_000) // 1000 seconds, arbitrary
	const bufferMs = 200

	cases := []struct {
		name          string
		staticDelayMs int
	}{
		{"no delay", 0},
		{"250ms delay", 250},
		{"1000ms delay", 1000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := sync.NewClockSync()
			sched := NewScheduler(cs, bufferMs, tc.staticDelayMs)
			defer sched.Stop()

			buf := audio.Buffer{Timestamp: serverTimestampUs}
			sched.Schedule(buf)

			// Schedule mutates a local copy; inspect the queue.
			sched.bufferMu.Lock()
			queued := sched.bufferQ.Peek()
			sched.bufferMu.Unlock()

			baseline := time.Unix(0, serverTimestampUs*1000)
			want := baseline.Add(time.Duration(tc.staticDelayMs) * time.Millisecond)

			if !queued.PlayAt.Equal(want) {
				t.Errorf("PlayAt = %v, want %v (static delay = %dms)",
					queued.PlayAt, want, tc.staticDelayMs)
			}
		})
	}
}

// TestScheduler_StaticDelayDefaultZero confirms that a scheduler built with
// the old two-argument shape would still behave correctly — i.e. the delay
// field defaults cleanly to 0. This is a belt-and-suspenders check against
// future refactors that might forget to thread the parameter through.
func TestScheduler_StaticDelayDefaultZero(t *testing.T) {
	cs := sync.NewClockSync()
	sched := NewScheduler(cs, 200, 0)
	defer sched.Stop()

	if sched.staticDelay != 0 {
		t.Errorf("staticDelay with 0 arg = %v, want 0", sched.staticDelay)
	}
}

// TestScheduler_StaticDelayClamped verifies that static_delay_ms values outside
// the spec range 0–5000 are clamped rather than applied verbatim, so a bad
// server value or local config cannot push playback arbitrarily out of sync.
func TestScheduler_StaticDelayClamped(t *testing.T) {
	cases := []struct {
		name   string
		in     int
		wantMs int
	}{
		{"negative clamps to zero", -100, 0},
		{"above max clamps to 5000", 9000, maxStaticDelayMs},
		{"at max is unchanged", maxStaticDelayMs, maxStaticDelayMs},
		{"in range is unchanged", 250, 250},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := sync.NewClockSync()
			sched := NewScheduler(cs, 200, tc.in)
			defer sched.Stop()

			want := time.Duration(tc.wantMs) * time.Millisecond
			if sched.staticDelay != want {
				t.Errorf("staticDelay = %v, want %v", sched.staticDelay, want)
			}
		})
	}
}

// ABOUTME: Desync-storm regression tests. When a client falls more than 50ms
// behind, every arriving chunk is "late" and the scheduler used to drop them
// one-by-one for the entire slip window (10s+ until the next clock-sync burst
// or stream restart rescued it) — the "few seconds of playback then a 10-15s
// cutout then resumption" failure. The scheduler now skips the backlog and
// re-enters buffering (desyncSkipThreshold) so playback re-synchronizes to
// live after roughly bufferMs instead.

func schedulePastChunks(s *Scheduler, ago time.Duration, n int) {
	// An unsynced ClockSync maps ServerToLocalTime deterministically to
	// time.Unix(0, ts*1000) == time.UnixMicro(ts), so a well-chosen server
	// timestamp puts every chunk's PlayAt exactly `ago` in the past.
	micros := time.Now().Add(-ago).UnixMicro()
	for i := 0; i < n; i++ {
		s.Schedule(audio.Buffer{Timestamp: micros})
	}
}

func drainSchedulerOutput(s *Scheduler, done chan<- int, stop <-chan struct{}) {
	n := 0
	for {
		select {
		case <-s.Output():
			n++
		case <-stop:
			done <- n
			return
		}
	}
}

// TestScheduler_DesyncStormReSyncsToLive simulates the reported failure: a
// backlog of chunks 1s past their play time. The scheduler must flush it,
// re-enter buffering, and then play fresh chunks again instead of staying
// silent until an external event.
func TestScheduler_DesyncStormReSyncsToLive(t *testing.T) {
	cs := sync.NewClockSync()
	sched := NewScheduler(cs, 200, 0) // bufferMs=200 → bufferTarget=10
	defer sched.Stop()

	done := make(chan int, 1)
	stop := make(chan struct{})
	defer close(stop)
	go drainSchedulerOutput(sched, done, stop)

	schedulePastChunks(sched, time.Second, 30)
	sched.processQueue()

	sched.bufferMu.Lock()
	reBuffering := sched.buffering
	qLen := sched.bufferQ.Len()
	sched.bufferMu.Unlock()
	if !reBuffering {
		t.Error("after storm: expected re-entered buffering")
	}
	if qLen != 0 {
		t.Errorf("after storm: expected empty queue, got %d", qLen)
	}
	if got := sched.played.Load(); got != 0 {
		t.Errorf("after storm: expected 0 played, got %d", got)
	}
	if got := sched.dropped.Load(); got != 30 {
		t.Errorf("after storm: expected 30 dropped, got %d", got)
	}

	// Fresh chunks mapping to ~now must play cleanly after the rebuffer.
	schedulePastChunks(sched, 0, 30)
	sched.processQueue()
	if got := sched.played.Load(); got != 30 {
		t.Errorf("after recovery: expected 30 played, got %d (dropped=%d)", got, sched.dropped.Load())
	}
}

// TestScheduler_NormalJitterDropDoesNotForceRebuffer guards the threshold: a
// chunk late by 60ms (beyond the 50ms per-chunk window, under the storm
// threshold) must be dropped per-chunk and must NOT trip the rebuffer.
func TestScheduler_NormalJitterDropDoesNotForceRebuffer(t *testing.T) {
	cs := sync.NewClockSync()
	sched := NewScheduler(cs, 200, 0)
	defer sched.Stop()

	schedulePastChunks(sched, 60*time.Millisecond, 30)
	sched.processQueue()

	sched.bufferMu.Lock()
	reBuffering := sched.buffering
	sched.bufferMu.Unlock()
	if reBuffering {
		t.Error("60ms-late chunks must not force a storm rebuffer")
	}
	if got := sched.dropped.Load(); got != 30 {
		t.Errorf("expected 30 late drops, got %d", got)
	}
	if got := sched.played.Load(); got != 0 {
		t.Errorf("expected 0 played, got %d", got)
	}
}

// TestScheduler_DesyncStormThresholdBoundary checks the storm trigger sits at
// desyncSkipThreshold: clearly-below lateness stays in per-chunk drop mode,
// clearly-above lateness trips the rebuffer.
func TestScheduler_DesyncStormThresholdBoundary(t *testing.T) {
	// ~240ms late: under the 250ms threshold even with scheduling elapsed time.
	csBelow := sync.NewClockSync()
	below := NewScheduler(csBelow, 200, 0)
	defer below.Stop()
	schedulePastChunks(below, 240*time.Millisecond, 12)
	below.processQueue()
	below.bufferMu.Lock()
	reBufBelow := below.buffering
	below.bufferMu.Unlock()
	if reBufBelow {
		t.Error("240ms late should not trip storm rebuffer")
	}

	// ~260ms late: over the threshold.
	csAbove := sync.NewClockSync()
	above := NewScheduler(csAbove, 200, 0)
	defer above.Stop()
	schedulePastChunks(above, 260*time.Millisecond, 12)
	above.processQueue()
	above.bufferMu.Lock()
	reBufAbove := above.buffering
	above.bufferMu.Unlock()
	if !reBufAbove {
		t.Error("260ms late should trip storm rebuffer")
	}
}
