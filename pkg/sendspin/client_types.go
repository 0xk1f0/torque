// ABOUTME: Shared client types — config, state, metadata, sentinel errors,
// ABOUTME: and small helpers used by the client (Receiver/Player).
package sendspin

import (
	"context"
	"errors"
	mrand "math/rand"
	"time"
)

// ChunkDurationMs is the fixed audio chunk length in milliseconds shared by
// every Sendspin implementation (20 ms, 50 chunks per second). It divides
// evenly into every supported sample rate.
const ChunkDurationMs = 20

// ReconnectConfig controls automatic reconnect behavior after the protocol
// connection drops. When Enabled is false (the zero value), the client behaves
// as a one-shot: a lost connection stays lost.
type ReconnectConfig struct {
	Enabled      bool
	InitialDelay time.Duration // default 500ms
	MaxDelay     time.Duration // default 30s
	Multiplier   float64       // default 2.0
	MaxAttempts  int           // 0 = infinite (default)

	// Rediscover is an optional callback invoked before each reconnect
	// attempt. Returning a non-empty address overrides the configured
	// ServerAddr for that attempt. Use it to re-run discovery when the
	// server may have moved. Errors are logged and the attempt falls back to
	// the last known address.
	Rediscover func(ctx context.Context) (string, error)
}

// DeviceInfo is the optional device identity advertised in client/hello.
type DeviceInfo struct {
	ProductName     string
	Manufacturer    string
	SoftwareVersion string
}

// Metadata is the current track's metadata, delivered via the OnMetadata
// callback. Values come from the server/state metadata object.
type Metadata struct {
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	ArtworkURL  string
	Track       int
	Year        int
	Duration    int    // seconds
	Repeat      string // "off", "one", "all"
	Shuffle     bool
}

// PlayerState is a snapshot of the client's playback state.
type PlayerState struct {
	State      string // "idle", "playing", "paused", "reconnecting"
	Volume     int
	Muted      bool
	Codec      string
	SampleRate int
	Channels   int
	BitDepth   int
	Connected  bool
}

// Graceful-degradation sentinel errors delivered through Config.OnError.
var (
	// ErrSyncLost is reported when clock-sync quality has been lost past the
	// grace window; playback pauses until recovery.
	ErrSyncLost = errors.New("clock sync lost: playback paused")
	// ErrSyncRecovered is reported when sync resumes after a loss.
	ErrSyncRecovered = errors.New("clock sync recovered: playback resumed")
	// ErrConnectionLost is reported when the encrypted connection drops.
	ErrConnectionLost = errors.New("server connection lost")
)

// jitter returns d scaled by ±frac for retry backoff.
func jitter(d time.Duration, frac float64) time.Duration {
	if d <= 0 {
		return d
	}
	delta := (mrand.Float64()*2 - 1) * frac
	return time.Duration(float64(d) * (1 + delta))
}
