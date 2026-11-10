// ABOUTME: The spec-v2 client — plays audio over the Noise-encrypted Sendspin
// ABOUTME: wire: handshake, session, clock sync, stream lifecycle, decode,
// ABOUTME: scheduling, sync-loss degradation, and reconnect.
package sendspin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/0xk1f0/spinsync/pkg/audio"
	"github.com/0xk1f0/spinsync/pkg/audio/decode"
	"github.com/0xk1f0/spinsync/pkg/audio/output"
	"github.com/0xk1f0/spinsync/pkg/protocol/playback"
	"github.com/0xk1f0/spinsync/pkg/protocol/secure"
	"github.com/0xk1f0/spinsync/pkg/protocol/session"
	"github.com/0xk1f0/spinsync/pkg/sync"
	"github.com/gorilla/websocket"
)

// V2TimeSyncBurstSize is the clock-sync burst size (spec: ≥8 per burst).
const V2TimeSyncBurstSize = 8

// Defaults for clock-sync loss monitoring on the v2 client (see V2Config).
const (
	defaultV2SyncMonitorInterval = 500 * time.Millisecond
	defaultV2SyncLossGrace       = 5 * time.Second
)

// V2Config configures the spec-v2 client (ReceiverV2 / PlayerV2).
type V2Config struct {
	// ServerAddr is the server address (host:port).
	ServerAddr string
	// Identity is the client's persistent Curve25519 keypair; the client_id
	// sent on the wire is its public key. It must be stable across reboots.
	Identity *secure.Identity
	// Name is the friendly name sent in client/hello.
	Name string

	BufferMs       int
	OutputDelayMs  int // output_delay_ms reported to the server (0–5000)
	BufferCapacity int // buffer_capacity advertised in player@v1_support
	PreferredCodec string
	MaxSampleRate  int
	MaxBitDepth    int

	// UnpairedAccess advertises whether the client admits unpaired servers.
	UnpairedAccess bool
	// ResolvePSK looks up a pairing record: given the server's psk_id and
	// psk_category from noise message 1, return the matching PSK (a psk_id
	// held under a different category is a miss, and the client falls back to
	// the Sentinel). Nil accepts only the Sentinel PSK (unpaired clients).
	ResolvePSK func(pskID string, category secure.PSKCategoryCode) (secure.PSK, bool)
	// PSKCategory classifies the PSK that matched the handshake and drives
	// the server/activate admissibility the client will accept. Defaults to
	// Sentinel (or long-term if ResolvePSK matched a record).
	PSKCategory session.PSKCategory

	// SyncMonitorInterval / SyncLossGrace control the clock-sync-loss
	// graceful-degradation monitor (same semantics as ReceiverConfig).
	SyncMonitorInterval time.Duration
	SyncLossGrace       time.Duration

	DeviceInfo     DeviceInfo
	DecoderFactory func(audio.Format) (decode.Decoder, error)
	OnMetadata     func(Metadata)
	OnStreamStart  func(audio.Format)
	OnStreamEnd    func()
	OnError        func(error)
	OnCommand      func(session.PlayerCommand)
	OnUnpair       func()

	// Output overrides the playback backend (PlayerV2 only). Nil = miniaudio.
	Output output.Output

	// OnStateChange receives player state transitions (PlayerV2 only).
	OnStateChange func(PlayerState)

	// Reconnect controls automatic reconnection (PlayerV2 only).
	Reconnect ReconnectConfig
}

// ReceiverV2 is the audio side of the v2 client: encrypted connect + session
// establishment, clock sync, stream lifecycle, decode and scheduling. It emits
// decoded audio.Buffer values on Output() exactly like the legacy Receiver, so
// it can be used by visualizers, DSP, or custom output backends.
type ReceiverV2 struct {
	config    V2Config
	conn      *secure.Conn
	sess      *session.ClientSession
	clockSync *sync.ClockSync

	ctx    context.Context
	cancel context.CancelFunc

	timeSyncResp chan session.ServerTime

	streamMu        stdsync.Mutex
	scheduler       *Scheduler
	schedulerCancel context.CancelFunc
	decoder         decode.Decoder
	format          audio.Format
	closingStreams  bool // under streamMu; excludes pump Add from Wait

	metadataMu stdsync.Mutex
	merged     Metadata

	stateMu   stdsync.Mutex
	available bool
	volume    int
	muted     bool

	output    chan audio.Buffer
	syncLost  atomic.Bool
	connected atomic.Bool
	pumpWG    stdsync.WaitGroup
}

// NewReceiverV2 builds a v2 client receiver. Identity is required.
func NewReceiverV2(cfg V2Config) (*ReceiverV2, error) {
	if cfg.ServerAddr == "" {
		return nil, fmt.Errorf("V2Config.ServerAddr is required")
	}
	if cfg.Identity == nil {
		return nil, fmt.Errorf("V2Config.Identity is required")
	}
	if cfg.BufferMs == 0 {
		cfg.BufferMs = 500
	}
	if cfg.BufferCapacity == 0 {
		cfg.BufferCapacity = 1048576
	}
	// Default to admitting unpaired servers so a fresh player (no pairing
	// record) is immediately playable; set false in strict deployments.
	if !cfg.UnpairedAccess {
		cfg.UnpairedAccess = true
	}
	if cfg.SyncMonitorInterval <= 0 {
		cfg.SyncMonitorInterval = defaultV2SyncMonitorInterval
	}
	if cfg.SyncLossGrace <= 0 {
		cfg.SyncLossGrace = defaultV2SyncLossGrace
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &ReceiverV2{
		config:       cfg,
		clockSync:    sync.NewClockSync(),
		ctx:          ctx,
		cancel:       cancel,
		timeSyncResp: make(chan session.ServerTime, 16),
		output:       make(chan audio.Buffer, 10),
	}, nil
}

// Output returns the channel emitting decoded, time-stamped audio buffers.
func (r *ReceiverV2) Output() <-chan audio.Buffer { return r.output }

// Done returns a channel closed when the connection is lost or the receiver
// is closed. Callers can use it to drive reconnect loops.
func (r *ReceiverV2) Done() <-chan struct{} { return r.ctx.Done() }

// ClockSync returns this receiver's time-filter instance.
func (r *ReceiverV2) ClockSync() *sync.ClockSync { return r.clockSync }

// Connect dials the server, runs the Noise handshake and session
// establishment, verifies the player role was activated, performs an initial
// clock-sync burst, and starts the background loops.
func (r *ReceiverV2) Connect() error {
	u := url.URL{Scheme: "ws", Host: r.config.ServerAddr, Path: "/sendspin"}
	log.Printf("V2 connecting to %s", u.String())

	ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", u.String(), err)
	}

	conn, _, err := secure.ClientHandshake(ws, secure.ClientHandshakeConfig{
		Identity:         r.config.Identity,
		HandshakeTimeout: 10 * time.Second,
		SelectPSK:        r.config.ResolvePSK,
	})
	if err != nil {
		ws.Close()
		return fmt.Errorf("noise handshake: %w", err)
	}

	hello, cat := r.buildHello()
	sess, err := session.EstablishClient(conn, session.ClientConfig{Hello: hello, PSKCategory: cat})
	if err != nil {
		conn.Close()
		return fmt.Errorf("session establish: %w", err)
	}
	if !hasRole(sess.ActiveRoles, "player") {
		conn.Close()
		return fmt.Errorf("server did not activate the player role (active_roles=%v)", sess.ActiveRoles)
	}

	r.conn = conn
	r.sess = sess
	r.connected.Store(true)
	log.Printf("V2 session established with %s (roles=%v)", sess.ServerName, sess.ActiveRoles)

	// Start the reader first so server/time replies are drained while the
	// initial clock-sync burst is in flight.
	go r.readLoop()
	go r.clockSyncLoop()
	go r.watchClockSync()

	// Per the spec a player sends its initial client/state only after clock
	// synchronization is established, so the server's stream timestamps are
	// trustworthy from the first chunk. Run bursts until the filter has a
	// sample (bounded), then go available.
	r.syncUntilReady(5 * time.Second)

	r.stateMu.Lock()
	r.available = true
	r.volume = 100
	r.stateMu.Unlock()
	return r.sendPlayerState()
}

// syncUntilReady runs clock-sync bursts until the time filter has produced at
// least one sample, or timeout elapses (a silent server just means the first
// chunks may be dropped; playback still begins).
func (r *ReceiverV2) syncUntilReady(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		r.runTimeSyncBurst(V2TimeSyncBurstSize)
		if rtt, _ := r.clockSync.GetStats(); rtt > 0 {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("V2 initial clock sync timed out; streaming with an unsynced filter")
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// buildHello assembles the client/hello for this connection.
func (r *ReceiverV2) buildHello() (session.ClientHello, session.PSKCategory) {
	formats := buildV2Formats(r.config.PreferredCodec, r.config.MaxSampleRate, r.config.MaxBitDepth)
	hello := session.ClientHello{
		Name: r.config.Name,
		DeviceInfo: &session.DeviceInfo{
			ProductName:     r.config.DeviceInfo.ProductName,
			Manufacturer:    r.config.DeviceInfo.Manufacturer,
			SoftwareVersion: r.config.DeviceInfo.SoftwareVersion,
			MACAddress:      pickStableMACOrEmpty(),
		},
		SupportedRoles: []string{"player@v1", "metadata@v1", "controller@v1"},
		PlayerSupport: &session.PlayerSupport{
			SupportedFormats: formats,
			BufferCapacity:   r.config.BufferCapacity,
		},
		SupportedPairMethods: session.SupportedPairMethods{string(session.PairMethodPairingPSK): {}},
		UnpairedAccess:       session.UnpairedAccess{Enabled: r.config.UnpairedAccess},
	}
	cat := r.config.PSKCategory
	if cat == 0 {
		// Default to long-term trust only when a pairing record matched the
		// handshake; otherwise the sentinel.
		cat = session.PSKSentinel
	}
	return hello, cat
}

func buildV2Formats(preferredCodec string, maxSampleRate, maxBitDepth int) []session.AudioFormat {
	all := []session.AudioFormat{
		{Codec: "pcm", Channels: 2, SampleRate: 192000, BitDepth: 24},
		{Codec: "pcm", Channels: 2, SampleRate: 176400, BitDepth: 24},
		{Codec: "pcm", Channels: 2, SampleRate: 96000, BitDepth: 24},
		{Codec: "pcm", Channels: 2, SampleRate: 88200, BitDepth: 24},
		{Codec: "pcm", Channels: 2, SampleRate: 48000, BitDepth: 16},
		{Codec: "pcm", Channels: 2, SampleRate: 44100, BitDepth: 16},
		{Codec: "flac", Channels: 2, SampleRate: 192000, BitDepth: 24},
		{Codec: "flac", Channels: 2, SampleRate: 96000, BitDepth: 24},
		{Codec: "flac", Channels: 2, SampleRate: 48000, BitDepth: 24},
		{Codec: "flac", Channels: 2, SampleRate: 44100, BitDepth: 16},
		{Codec: "opus", Channels: 2, SampleRate: 48000, BitDepth: 16},
	}
	filtered := make([]session.AudioFormat, 0, len(all))
	for _, f := range all {
		if maxSampleRate > 0 && f.SampleRate > maxSampleRate {
			continue
		}
		if maxBitDepth > 0 && f.BitDepth > maxBitDepth {
			continue
		}
		filtered = append(filtered, f)
	}
	if preferredCodec == "" {
		return filtered
	}
	preferred := make([]session.AudioFormat, 0, len(filtered))
	rest := make([]session.AudioFormat, 0, len(filtered))
	for _, f := range filtered {
		if f.Codec == preferredCodec {
			preferred = append(preferred, f)
		} else {
			rest = append(rest, f)
		}
	}
	return append(preferred, rest...)
}

func hasRole(roles []string, family string) bool {
	for _, role := range roles {
		if role == family || len(role) > len(family) && role[:len(family)+1] == family+"@" {
			return true
		}
	}
	return false
}

// SetVolume stores a local volume change and reports it to the server via
// client/state (the server merges it into the group volume). Muted volume
// changes are folded in but the mute bit stays set, per spec.
func (r *ReceiverV2) SetVolume(volume int) {
	if volume < 0 {
		volume = 0
	}
	if volume > 100 {
		volume = 100
	}
	r.stateMu.Lock()
	r.volume = volume
	r.stateMu.Unlock()
	_ = r.sendPlayerState()
}

// SetMuted stores a local mute change and reports it to the server.
func (r *ReceiverV2) SetMuted(muted bool) {
	r.stateMu.Lock()
	r.muted = muted
	r.stateMu.Unlock()
	_ = r.sendPlayerState()
}

// sendPlayerState reports the player's full state (available, volume, mute,
// timing) — the spec requires `available` on every client/state.
func (r *ReceiverV2) sendPlayerState() error {
	if r.conn == nil {
		return errors.New("not connected")
	}
	r.stateMu.Lock()
	avail := r.available
	vol := r.volume
	mut := r.muted
	od := r.config.OutputDelayMs
	minb := r.config.BufferMs
	r.stateMu.Unlock()

	state := session.ClientState{
		Available: avail,
		Player: &session.ClientPlayerState{
			OutputDelayMs: &od,
			MinBufferMs:   ptr(minb),
			Volume:        &vol,
			Muted:         &mut,
		},
	}
	raw, err := session.Marshal(&state)
	if err != nil {
		return err
	}
	return r.conn.WriteMessage(secure.MsgTypeJSON, raw)
}

func ptr[T any](v T) *T { return &v }

// readLoop drives the connection: JSON controls and binary role frames until
// an error ends the connection.
func (r *ReceiverV2) readLoop() {
	defer r.teardown()

	for {
		msgType, payload, err := r.conn.ReadMessage()
		if err != nil {
			log.Printf("V2 read error: %v", err)
			return
		}
		switch msgType {
		case secure.MsgTypeJSON:
			r.handleControl(payload)
		case playback.PlayerAudioMsgType:
			r.handleAudioChunk(payload)
		default:
			// Unknown/other-role binary frames are ignored per the spec.
		}
	}
}

func (r *ReceiverV2) handleControl(payload []byte) {
	msg, err := session.Parse(payload)
	if err != nil {
		log.Printf("V2 parse control error: %v", err)
		return
	}
	switch v := msg.(type) {
	case *session.ServerTime:
		select {
		case r.timeSyncResp <- *v:
		default: // drop on a full queue; next burst re-syncs
		}
	case *session.StreamStart:
		if v.Player != nil {
			r.handleStreamStart(*v.Player)
		}
	case *session.StreamClear:
		r.handleStreamClear(*v)
	case *session.StreamEnd:
		r.handleStreamEnd()
	case *session.ServerState:
		r.handleServerState(v)
	case *session.ServerCommand:
		if v.Player != nil && r.config.OnCommand != nil {
			r.config.OnCommand(*v.Player)
		}
	case *session.GroupUpdate:
		log.Printf("V2 group update: %s (%s)", v.GroupName, v.PlaybackState)
	case *session.ServerUnpair:
		log.Printf("V2 server unpairing this client")
		if r.config.OnUnpair != nil {
			r.config.OnUnpair()
		}
	default:
		// Forward tolerance: ignore unrecognized messages.
	}
}

// handleStreamStart opens a decoder + scheduler for the negotiated format.
func (r *ReceiverV2) handleStreamStart(sp session.StreamStartPlayer) {
	format := audio.Format{
		Codec:      sp.Codec,
		SampleRate: sp.SampleRate,
		Channels:   sp.Channels,
		BitDepth:   sp.BitDepth,
	}
	if sp.CodecHeader != "" {
		if hb, err := base64.StdEncoding.DecodeString(sp.CodecHeader); err == nil {
			format.CodecHeader = hb
		}
	}

	var decoder decode.Decoder
	var err error
	if r.config.DecoderFactory != nil {
		decoder, err = r.config.DecoderFactory(format)
	} else {
		decoder, err = defaultDecoderFor(format)
	}
	if err != nil {
		r.notify(fmt.Errorf("v2: create decoder for %s: %w", format.Codec, err))
		return
	}
	if r.config.OnStreamStart != nil {
		r.config.OnStreamStart(format)
	}

	newCtx, newCancel := context.WithCancel(r.ctx)
	newScheduler := NewScheduler(r.clockSync, r.config.BufferMs, r.config.OutputDelayMs)

	r.streamMu.Lock()
	if r.closingStreams {
		r.streamMu.Unlock()
		newCancel()
		newScheduler.Stop()
		return
	}
	oldScheduler := r.scheduler
	oldCancel := r.schedulerCancel
	r.scheduler = newScheduler
	r.schedulerCancel = newCancel
	r.decoder = decoder
	r.format = format
	r.pumpWG.Add(1)
	r.streamMu.Unlock()

	if oldCancel != nil {
		oldCancel()
	}
	if oldScheduler != nil {
		oldScheduler.Stop()
	}

	go newScheduler.Run()
	go r.pumpSchedulerOutputV2(newCtx, newScheduler)
}

func (r *ReceiverV2) handleStreamClear(clear session.StreamClear) {
	r.streamMu.Lock()
	sched := r.scheduler
	r.streamMu.Unlock()
	if sched == nil {
		return
	}
	if clear.FromTimestamp != nil {
		sched.ClearSince(*clear.FromTimestamp)
	} else {
		sched.Clear()
	}
}

func (r *ReceiverV2) handleStreamEnd() {
	if r.config.OnStreamEnd != nil {
		r.config.OnStreamEnd()
	}
	r.stopStream()
}

// handleAudioChunk decodes one player-audio message body (12-byte header
// after the type byte) and schedules it. send_ahead carries no scheduling
// meaning — it is only a delay sample, so it is parsed and ignored.
func (r *ReceiverV2) handleAudioChunk(body []byte) {
	ts, _, audioData, err := playback.ParseAudioChunk(body)
	if err != nil {
		r.notify(fmt.Errorf("v2: malformed audio chunk: %w", err))
		return
	}

	r.streamMu.Lock()
	decoder := r.decoder
	sched := r.scheduler
	format := r.format
	r.streamMu.Unlock()
	if decoder == nil || sched == nil {
		return
	}

	pcm, err := decoder.Decode(audioData)
	if err != nil {
		r.notify(fmt.Errorf("v2: decode error: %w", err))
		return
	}
	if len(pcm) == 0 {
		return
	}
	sched.Schedule(audio.Buffer{Timestamp: ts, Samples: pcm, Format: format})
}

func (r *ReceiverV2) handleServerState(st *session.ServerState) {
	if st.Metadata == nil {
		return
	}
	r.metadataMu.Lock()
	m := &r.merged
	if st.Metadata.Timestamp > 0 {
		// simple snapshot semantics; diff-merging a future-timestamp update
		// is a follow-up.
	}
	if st.Metadata.Title != nil {
		m.Title = *st.Metadata.Title
	}
	if st.Metadata.Artist != nil {
		m.Artist = *st.Metadata.Artist
	}
	if st.Metadata.AlbumArtist != nil {
		m.AlbumArtist = *st.Metadata.AlbumArtist
	}
	if st.Metadata.Album != nil {
		m.Album = *st.Metadata.Album
	}
	if st.Metadata.ArtworkURL != nil {
		m.ArtworkURL = *st.Metadata.ArtworkURL
	}
	if st.Metadata.Year != nil {
		m.Year = *st.Metadata.Year
	}
	if st.Metadata.Track != nil {
		m.Track = *st.Metadata.Track
	}
	if st.Metadata.Progress != nil {
		m.Duration = st.Metadata.Progress.TrackDuration / 1000
	}
	snapshot := r.merged
	r.metadataMu.Unlock()

	if r.config.OnMetadata != nil {
		r.config.OnMetadata(snapshot)
	}
}

// runTimeSyncBurst drives a sequential burst of client/time probes and feeds
// the lowest-RTT sample into the time filter, per the spec.
func (r *ReceiverV2) runTimeSyncBurst(size int) {
drain:
	for {
		select {
		case <-r.timeSyncResp:
		default:
			break drain
		}
	}

	var (
		bestT1, bestT2, bestT3, bestT4 int64
		bestRTT                        = int64(math.MaxInt64)
		valid                          int
	)
	for i := 0; i < size; i++ {
		t1 := time.Now().UnixMicro()
		raw, err := session.Marshal(&session.ClientTime{ClientTransmitted: t1})
		if err != nil {
			log.Printf("V2 sync marshal: %v", err)
			continue
		}
		if err := r.conn.WriteMessage(secure.MsgTypeJSON, raw); err != nil {
			log.Printf("V2 sync send %d/%d failed: %v", i+1, size, err)
			continue
		}
		select {
		case resp := <-r.timeSyncResp:
			t4 := time.Now().UnixMicro()
			rtt := (t4 - resp.ClientTransmitted) - (resp.ServerTransmitted - resp.ServerReceived)
			if rtt < bestRTT {
				bestRTT = rtt
				bestT1 = resp.ClientTransmitted
				bestT2 = resp.ServerReceived
				bestT3 = resp.ServerTransmitted
				bestT4 = t4
			}
			valid++
		case <-time.After(500 * time.Millisecond):
			log.Printf("V2 sync sample %d/%d timed out", i+1, size)
		case <-r.ctx.Done():
			return
		}
	}
	if valid == 0 {
		return
	}
	r.clockSync.ProcessSyncResponse(bestT1, bestT2, bestT3, bestT4)
}

func (r *ReceiverV2) clockSyncLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.runTimeSyncBurst(V2TimeSyncBurstSize)
		case <-r.ctx.Done():
			return
		}
	}
}

// watchClockSync implements graceful degradation on clock-sync loss (same
// semantics as the legacy Receiver).
func (r *ReceiverV2) watchClockSync() {
	tick := r.config.SyncMonitorInterval
	grace := r.config.SyncLossGrace
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	paused := false
	for {
		select {
		case <-ticker.C:
			if r.clockSync.CheckQuality() != sync.QualityLost {
				if paused {
					paused = false
					r.resumePlayback()
				}
				continue
			}
			if paused || !r.clockSync.Synced() || r.clockSync.TimeSinceLastSync() < grace {
				continue
			}
			paused = true
			r.pausePlayback()
		case <-r.ctx.Done():
			return
		}
	}
}

func (r *ReceiverV2) pausePlayback() {
	r.streamMu.Lock()
	sched := r.scheduler
	r.streamMu.Unlock()
	if sched != nil {
		sched.Clear()
	}
	r.syncLost.Store(true)
	r.notify(ErrSyncLost)
}

func (r *ReceiverV2) resumePlayback() {
	r.streamMu.Lock()
	sched := r.scheduler
	r.streamMu.Unlock()
	if sched != nil {
		sched.Clear()
	}
	r.syncLost.Store(false)
	r.notify(ErrSyncRecovered)
}

func (r *ReceiverV2) pumpSchedulerOutputV2(ctx context.Context, sched *Scheduler) {
	defer r.pumpWG.Done()
	for {
		select {
		case buf := <-sched.Output():
			if r.syncLost.Load() {
				continue
			}
			select {
			case r.output <- buf:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// stopStream tears down the active scheduler/decoder without closing output.
func (r *ReceiverV2) stopStream() {
	r.streamMu.Lock()
	sched := r.scheduler
	dec := r.decoder
	pc := r.schedulerCancel
	r.scheduler = nil
	r.decoder = nil
	r.schedulerCancel = nil
	r.streamMu.Unlock()
	if pc != nil {
		pc()
	}
	if sched != nil {
		sched.Stop()
	}
	if dec != nil {
		_ = dec.Close()
	}
}

// teardown runs the drop/close path: stops the stream, cancels everything,
// and notifies.
func (r *ReceiverV2) teardown() {
	r.connected.Store(false)
	r.stopStream()
	r.cancel()
	r.notify(ErrConnectionLost)
}

// notify delivers an error to OnError or the log.
func (r *ReceiverV2) notify(err error) {
	if r.config.OnError != nil {
		r.config.OnError(err)
	} else {
		log.Printf("V2 client: %v", err)
	}
}

// Close shuts the receiver down, free any stream first, then the connection.
func (r *ReceiverV2) Close() error {
	if r.conn != nil {
		_ = writeV2Goodbye(r.conn, session.GoodbyeShutdown)
	}
	r.streamMu.Lock()
	r.closingStreams = true
	r.streamMu.Unlock()
	r.stopStream()
	r.cancel()
	if r.conn != nil {
		_ = r.conn.Close()
	}
	// Wait for any in-flight pump before closing output.
	r.pumpWG.Wait()
	r.connected.Store(false)
	select {
	case <-r.ctx.Done():
	default:
		// ctx already cancelled
	}
	close(r.output)
	return nil
}

func writeV2Goodbye(conn *secure.Conn, reason session.GoodbyeReason) error {
	raw, err := session.Marshal(&session.ClientGoodbye{Reason: reason})
	if err != nil {
		return err
	}
	return conn.WriteMessage(secure.MsgTypeJSON, raw)
}

// SendAvailability re-reports availability (e.g. entering/leaving external
// source mode).
func (r *ReceiverV2) SendAvailability(available bool) error {
	r.stateMu.Lock()
	r.available = available
	r.stateMu.Unlock()
	return r.sendPlayerState()
}

// SendControllerCommand sends a controller command (client/command).
func (r *ReceiverV2) SendControllerCommand(cmd session.ControllerCommand) error {
	if !cmd.Command.Valid() {
		return fmt.Errorf("invalid controller command %q", cmd.Command)
	}
	if err := cmd.Validate(); err != nil {
		return err
	}
	raw, err := session.Marshal(&session.ClientCommand{Controller: &cmd})
	if err != nil {
		return err
	}
	return r.conn.WriteMessage(secure.MsgTypeJSON, raw)
}

// defaultDecoderFor maps a v2 wire format to its decoder.
func defaultDecoderFor(format audio.Format) (decode.Decoder, error) {
	switch format.Codec {
	case "pcm":
		return decode.NewPCM(format)
	case "opus":
		return decode.NewOpus(format)
	case "flac":
		return decode.NewFLAC(format)
	default:
		return nil, fmt.Errorf("unsupported codec: %s", format.Codec)
	}
}
