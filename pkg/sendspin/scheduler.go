// ABOUTME: Timestamp-based playback scheduler for pkg/sendspin
// ABOUTME: Schedules audio buffers for precise playback timing
package sendspin

import (
	"container/heap"
	"context"
	"fmt"
	"log"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/0xk1f0/torque/pkg/audio"
	"github.com/0xk1f0/torque/pkg/sync"
)

// maxStaticDelayMs is the upper bound the Sendspin spec defines for
// static_delay_ms (integer, range 0–5000).
const maxStaticDelayMs = 5000

// desyncSkipThreshold is how far past its play time the head-of-queue chunk
// may fall before the scheduler treats it as a desync storm and re-buffers
// rather than dropping one late chunk per tick (which would keep the client
// silent for the whole slip window, until a clock burst or stream restart).
const desyncSkipThreshold = 250 * time.Millisecond

type Scheduler struct {
	clockSync    *sync.ClockSync
	bufferQ      *BufferQueue
	bufferMu     gosync.Mutex // Protects bufferQ and buffering
	output       chan audio.Buffer
	jitterMs     int
	staticDelay  time.Duration // shifts scheduled play time forward to compensate for hardware latency
	ctx          context.Context
	cancel       context.CancelFunc
	buffering    bool
	bufferTarget int // Number of chunks to buffer before starting playback

	received atomic.Int64
	played   atomic.Int64
	dropped  atomic.Int64
}

type SchedulerStats struct {
	Received int64
	Played   int64
	Dropped  int64
}

// NewScheduler creates a playback scheduler.
// bufferMs controls startup buffering (ms of audio to accumulate before playback).
// staticDelayMs shifts every scheduled play time forward, compensating for
// downstream hardware latency like Bluetooth sinks or AVRs. Zero means no shift.
// Per the Sendspin spec static_delay_ms is an integer in the range 0–5000;
// out-of-range values are clamped so a misbehaving server or bad local config
// cannot push playback arbitrarily far out of sync.
func NewScheduler(clockSync *sync.ClockSync, bufferMs int, staticDelayMs int) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())

	if staticDelayMs < 0 || staticDelayMs > maxStaticDelayMs {
		clamped := staticDelayMs
		if clamped < 0 {
			clamped = 0
		} else {
			clamped = maxStaticDelayMs
		}
		log.Printf("static_delay_ms %d out of range [0,%d], clamping to %d",
			staticDelayMs, maxStaticDelayMs, clamped)
		staticDelayMs = clamped
	}

	// Calculate buffer target from user config (bufferMs / ChunkDurationMs)
	bufferTarget := bufferMs / ChunkDurationMs
	if bufferTarget < 1 {
		bufferTarget = 1 // Minimum 1 chunk
	}

	return &Scheduler{
		clockSync:    clockSync,
		bufferQ:      NewBufferQueue(),
		output:       make(chan audio.Buffer, 10),
		jitterMs:     bufferMs, // Store for potential future use
		staticDelay:  time.Duration(staticDelayMs) * time.Millisecond,
		ctx:          ctx,
		cancel:       cancel,
		buffering:    true,
		bufferTarget: bufferTarget,
	}
}

func (s *Scheduler) Schedule(buf audio.Buffer) {
	buf.PlayAt = s.clockSync.ServerToLocalTime(buf.Timestamp).Add(s.staticDelay)

	received := s.received.Add(1) - 1

	// Sanity logs for first 5 chunks showing timing
	if received < 5 {
		serverNow := s.clockSync.ServerMicrosNow()
		diff := buf.Timestamp - serverNow
		rtt, quality := s.clockSync.GetStats()

		log.Printf("Chunk #%d: timestamp=%dµs, serverNow=%dµs, diff=%dµs (%.1fms), rtt=%dµs, quality=%v",
			received, buf.Timestamp, serverNow, diff, float64(diff)/1000.0, rtt, quality)
	}

	s.bufferMu.Lock()
	heap.Push(s.bufferQ, buf)
	s.bufferMu.Unlock()
}

func (s *Scheduler) Run() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	// Periodic one-line health summary so late-drop storms and silent stalls
	// are visible in the timeline instead of requiring per-chunk scrolling.
	tick := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.processQueue()
			tick++
			if tick%500 == 0 {
				rtt, quality := s.clockSync.GetStats()
				log.Printf("Scheduler stats: received=%d played=%d dropped=%d buffered=%dms rtt=%dµs quality=%v sync_age=%s",
					s.received.Load(), s.played.Load(), s.dropped.Load(), s.BufferDepth(),
					rtt, quality, s.clockSync.TimeSinceLastSync())
			}
		}
	}
}

func (s *Scheduler) processQueue() {
	s.bufferMu.Lock()

	if s.buffering {
		if s.bufferQ.Len() >= s.bufferTarget {
			log.Printf("Startup buffering complete: %d chunks ready", s.bufferQ.Len())
			s.buffering = false
		} else {
			s.bufferMu.Unlock()
			return
		}
	}

	now := time.Now()

	// Desync storm: the head chunk is far past its play time, not the ±50ms
	// jitter the per-chunk drop below bounds. From here every arriving chunk is
	// also late (the stream advances in real time), so dropping one per tick
	// would keep the client silent for the whole slip window. Skip the backlog
	// and re-enter buffering instead — same machinery Clear() uses for seeks —
	// limiting the outage to ~bufferMs.
	if s.bufferQ.Len() > 0 {
		if head := s.bufferQ.Peek(); now.Sub(head.PlayAt) > desyncSkipThreshold {
			flushed := s.bufferQ.Len()
			s.bufferQ = NewBufferQueue()
			s.buffering = true
			s.dropped.Add(int64(flushed))
			log.Printf("Scheduler: playback fell behind by %v; skipping %d late chunks and re-buffering (~%dms)%s",
				now.Sub(head.PlayAt), flushed, s.bufferTarget*ChunkDurationMs,
				s.clockContext(head.Timestamp))
			s.bufferMu.Unlock()
			return
		}
	}

	for s.bufferQ.Len() > 0 {
		buf := s.bufferQ.Peek()

		delay := buf.PlayAt.Sub(now)

		if delay > 50*time.Millisecond {
			// Too early — wait for next tick
			break
		} else if delay < -50*time.Millisecond {
			// More than 50ms late: drop rather than play out of sync
			heap.Pop(s.bufferQ)
			s.dropped.Add(1)
			log.Printf("Dropped late buffer: %v late%s", -delay, s.clockContext(buf.Timestamp))
		} else {
			// Within ±50ms window: ready to play
			heap.Pop(s.bufferQ)
			// Unlock before sending to avoid blocking while holding lock
			s.bufferMu.Unlock()

			select {
			case s.output <- buf:
				s.played.Add(1)
			case <-s.ctx.Done():
				return
			}

			s.bufferMu.Lock()
		}
	}

	s.bufferMu.Unlock()
}

// clockContext is a diagnostic suffix for late-drop lines: the chunk's lead in
// the server's frame, and the clock-sync RTT/quality/age. A large positive
// server_lead on a dropped chunk means the timestamp mapping is off; a ~zero
// lead with fresh sync points at the server watermark or pipeline latency.
func (s *Scheduler) clockContext(chunkTS int64) string {
	leadUs := chunkTS - s.clockSync.ServerMicrosNow()
	rtt, quality := s.clockSync.GetStats()
	return fmt.Sprintf(" (server_lead=%dµs, rtt=%dµs, quality=%v, sync_age=%s)",
		leadUs, rtt, quality, s.clockSync.TimeSinceLastSync())
}

func (s *Scheduler) Output() <-chan audio.Buffer {
	return s.output
}

func (s *Scheduler) Stats() SchedulerStats {
	return SchedulerStats{
		Received: s.received.Load(),
		Played:   s.played.Load(),
		Dropped:  s.dropped.Load(),
	}
}

// BufferDepth returns the current buffer queue depth in milliseconds
func (s *Scheduler) BufferDepth() int {
	s.bufferMu.Lock()
	depth := s.bufferQ.Len() * ChunkDurationMs
	s.bufferMu.Unlock()
	return depth
}

func (s *Scheduler) Stop() {
	s.cancel()
}

// Clear clears all buffered audio (used for seek operations)
func (s *Scheduler) Clear() {
	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()
	s.bufferQ = NewBufferQueue()
	s.buffering = true
	log.Printf("Scheduler buffers cleared, re-entering buffering mode")
}

// ClearSince drops buffered audio at or after fromTimestamp (server clock), the
// spec's stream/clear from_timestamp semantics: audio strictly before the time
// keeps playing, at-or-after is discarded.
func (s *Scheduler) ClearSince(fromTimestamp int64) {
	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()

	items := make([]audio.Buffer, 0, s.bufferQ.Len())
	for s.bufferQ.Len() > 0 {
		items = append(items, heap.Pop(s.bufferQ).(audio.Buffer))
	}
	kept := make([]audio.Buffer, 0, len(items))
	dropped := 0
	for _, b := range items {
		if b.Timestamp >= fromTimestamp {
			dropped++
			continue
		}
		kept = append(kept, b)
	}
	s.bufferQ = NewBufferQueue()
	for _, b := range kept {
		heap.Push(s.bufferQ, b)
	}
	s.dropped.Add(int64(dropped))
	log.Printf("Scheduler cleared %d buffered chunks at/after ts=%dµs; %d kept",
		dropped, fromTimestamp, len(kept))
}

type BufferQueue struct {
	items []audio.Buffer
}

func NewBufferQueue() *BufferQueue {
	q := &BufferQueue{}
	heap.Init(q)
	return q
}

// Implement heap.Interface
func (q *BufferQueue) Len() int { return len(q.items) }

func (q *BufferQueue) Less(i, j int) bool {
	// Bounds check to prevent crashes
	if i >= len(q.items) || j >= len(q.items) {
		return false
	}
	return q.items[i].PlayAt.Before(q.items[j].PlayAt)
}

func (q *BufferQueue) Swap(i, j int) {
	// Bounds check to prevent crashes
	if i >= len(q.items) || j >= len(q.items) {
		return
	}
	q.items[i], q.items[j] = q.items[j], q.items[i]
}

func (q *BufferQueue) Push(x interface{}) {
	q.items = append(q.items, x.(audio.Buffer))
}

func (q *BufferQueue) Pop() interface{} {
	n := len(q.items)
	if n == 0 {
		return audio.Buffer{}
	}
	item := q.items[n-1]
	q.items = q.items[:n-1]
	return item
}

func (q *BufferQueue) Peek() audio.Buffer {
	if len(q.items) == 0 {
		return audio.Buffer{}
	}
	return q.items[0]
}
