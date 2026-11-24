// ABOUTME: The client — plays audio over the Noise-encrypted Sendspin
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

	"github.com/0xk1f0/torque/pkg/audio"
	"github.com/0xk1f0/torque/pkg/audio/decode"
	"github.com/0xk1f0/torque/pkg/audio/output"
	"github.com/0xk1f0/torque/pkg/protocol/playback"
	"github.com/0xk1f0/torque/pkg/protocol/secure"
	"github.com/0xk1f0/torque/pkg/protocol/session"
	"github.com/0xk1f0/torque/pkg/sync"
	"github.com/coder/websocket"
)

// TimeSyncBurstSize is the clock-sync burst size (spec: ≥8 per burst).
const TimeSyncBurstSize = 8

// Defaults for clock-sync loss monitoring on the client (see Config).
const (
	defaultSyncMonitorInterval = 500 * time.Millisecond
	// SyncLossGrace must comfortably exceed the clock-sync cadence
	// (clockSyncLoop bursts every 10s). A grace shorter than the burst interval
	// makes the receiver go stale mid-cycle and oscillate pause/resume: it syncs
	// at t=0, the filter times out at t≈grace, playback pauses, then resumes at
	// the next burst only to pause again. 30s is three burst intervals, so a
	// live connection stays fresh and loss is declared only for a genuinely dead
	// server (30s with no client/time reply).
	defaultSyncLossGrace = 30 * time.Second

	// decodeQueueDepth bounds the raw-chunk pipeline between the read loop and
	// the decode worker (~2.5s of 20ms chunks): reads pause only when decode
	// falls that far behind, instead of stalling after every chunk.
	decodeQueueDepth = 128
)

// decodeJob is one raw chunk queued for the decode worker. StreamGen ties it to
// the stream generation that parsed it, so stale chunks are dropped on
// stream/start.
type decodeJob struct {
	StreamGen uint64
	Timestamp int64
	Data      []byte
	Format    audio.Format
}

// Config configures the client (Receiver / Player).
type Config struct {
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

	// UnpairedAccess advertises whether the client admits unpaired servers:
	// an unpaired (Sentinel-psk) server may open a direct playback session
	// without pairing. Off by default — a fresh client requires dynamic
	// pin-code pairing before audio plays. Enable it only for servers that
	// do not support pairing (or when the operator intentionally skips it);
	// a server may still choose to pair even when the client admits
	// unpaired access.
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
	// Store persists all client state tied to the identity — the long-term
	// PSK minted by dynamic pairing-code pairing (so a paired client
	// reconnects without re-pairing) and the player volume/mute. Required to
	// complete pairing.
	Store ClientStore
	// PairingCodeEmitted receives the dynamic pairing code (6 digits for
	// 'digits', an "SP: v1" token for 'qr_code') during dynamic pairing-code
	// pairing, for the operator to enter on the server. Nil logs it.
	PairingCodeEmitted func(code string)

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

	// Output overrides the playback backend (Player only). Nil = miniaudio.
	Output output.Output

	// OnStateChange receives player state transitions (Player only).
	OnStateChange func(PlayerState)

	// Reconnect controls automatic reconnection (Player only).
	Reconnect ReconnectConfig
}

// Receiver is the audio side of the client: encrypted connect + session
// establishment, clock sync, stream lifecycle, decode and scheduling. It emits
// decoded audio.Buffer values on Output() exactly like the legacy Receiver, so
// it can be used by visualizers, DSP, or custom output backends.
type Receiver struct {
	config    Config
	conn      *secure.Conn
	sess      *session.ClientSession
	clockSync *sync.ClockSync

	ctx    context.Context
	cancel context.CancelFunc

	timeSyncResp chan session.ServerTime

	// syncBurstMu serializes clock-sync bursts. Both clockSyncLoop (periodic)
	// and activatePlayer's syncUntilReady can race to run a burst; without a
	// lock they send probes concurrently into the shared timeSyncResp channel,
	// stealing each other's replies so both come up empty (logged as duplicated
	// "sync sample timed out" lines and a multi-second RTT). The periodic loop
	// is the steady-state owner, so a contender that loses the TryLock just
	// yields rather than queue.
	syncBurstMu stdsync.Mutex

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

	// matchedCat is the PSK category the handshake resolver selected for this
	// connection (set by defaultResolvePSK; 0 when the caller supplied a
	// custom ResolvePSK). guarded by stateMu.
	matchedCat session.PSKCategory
	// cat is the PSK category the current session is keyed on: the category
	// used to establish the session (Sentinel unless a pairing record
	// matched), promoted to long-term after a successful pairing. It is what
	// every server/activate, including re-sent ones, is adjudicated under.
	// guarded by stateMu.
	cat session.PSKCategory
	// paired records that this connection went through the pairing_psk flow.
	// guarded by stateMu.
	paired bool
	// hello is the client/hello this connection advertised; retained so a
	// pairing activation arriving after the initial activate can re-run the
	// pairing flow. guarded by stateMu.
	hello session.ClientHello
	// pairingIndex counts pairing server/activate messages seen since the last
	// Noise handshake; each pairing activate consumes the next index. The first
	// pairing activate (even when not the first overall) has index 1. guarded
	// by stateMu.
	pairingIndex int

	output    chan audio.Buffer
	syncLost  atomic.Bool
	connected atomic.Bool

	// decodeCh queues raw chunks for the decode worker, keeping codec CPU work
	// off the read loop. Bounded by decodeQueueDepth.
	decodeCh chan decodeJob
	// streamGen counts stream/start generations: async-decoded chunks are
	// scheduled only if their generation still matches the active stream.
	streamGen uint64
	// pairingActive is true while a PIN pairing flow has exclusive use of the
	// connection (it runs on the single readLoop goroutine and reads frames
	// directly). While set, clockSyncLoop withholds new bursts so their
	// server/time replies never interleave with pairing messages.
	pairingActive atomic.Bool
	pumpWG        stdsync.WaitGroup
}

// NewReceiver builds a client receiver. Identity is required.
func NewReceiver(cfg Config) (*Receiver, error) {
	if cfg.ServerAddr == "" {
		return nil, fmt.Errorf("Config.ServerAddr is required")
	}
	if cfg.Identity == nil {
		return nil, fmt.Errorf("Config.Identity is required")
	}
	if cfg.BufferMs == 0 {
		cfg.BufferMs = 500
	}
	if cfg.BufferCapacity == 0 {
		cfg.BufferCapacity = 1048576
	}
	if cfg.SyncMonitorInterval <= 0 {
		cfg.SyncMonitorInterval = defaultSyncMonitorInterval
	}
	if cfg.SyncLossGrace <= 0 {
		cfg.SyncLossGrace = defaultSyncLossGrace
	}
	// The ClockSync staleness window must match the graceful-degradation grace,
	// else CheckQuality reports QualityLost partway through each clock-sync
	// cycle (see defaultSyncLossGrace) and the monitor oscillates pause/resume.
	clockSync := sync.NewClockSync()
	clockSync.SetStaleAfter(cfg.SyncLossGrace)
	ctx, cancel := context.WithCancel(context.Background())
	return &Receiver{
		config:       cfg,
		clockSync:    clockSync,
		ctx:          ctx,
		cancel:       cancel,
		timeSyncResp: make(chan session.ServerTime, 16),
		output:       make(chan audio.Buffer, 10),
		decodeCh:     make(chan decodeJob, decodeQueueDepth),
	}, nil
}

// Output returns the channel emitting decoded, time-stamped audio buffers.
func (r *Receiver) Output() <-chan audio.Buffer { return r.output }

// Done returns a channel closed when the connection is lost or the receiver
// is closed. Callers can use it to drive reconnect loops.
func (r *Receiver) Done() <-chan struct{} { return r.ctx.Done() }

// ClockSync returns this receiver's time-filter instance.
func (r *Receiver) ClockSync() *sync.ClockSync { return r.clockSync }

// Connect dials the server, runs the Noise handshake and session
// establishment, verifies the player role was activated, performs an initial
// clock-sync burst, and starts the background loops.
func (r *Receiver) Connect() error {
	u := url.URL{Scheme: "ws", Host: r.config.ServerAddr, Path: "/sendspin"}
	log.Printf("connecting to %s", u.String())

	ws, _, err := websocket.Dial(r.ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", u.String(), err)
	}

	resolve := r.config.ResolvePSK
	if resolve == nil {
		resolve = r.defaultResolvePSK
	}
	conn, _, err := secure.ClientHandshake(ws, secure.ClientHandshakeConfig{
		Identity:         r.config.Identity,
		HandshakeTimeout: 10 * time.Second,
		SelectPSK:        resolve,
	})
	if err != nil {
		ws.Close(websocket.StatusNormalClosure, "")
		return fmt.Errorf("noise handshake: %w", err)
	}

	hello, cat := r.buildHello()
	sess, err := session.EstablishClient(conn, session.ClientConfig{Hello: hello, PSKCategory: cat})
	if err != nil {
		conn.Close()
		return fmt.Errorf("session establish: %w", err)
	}
	if !hasRole(sess.ActiveRoles, "player") {
		log.Printf("server/activate granted no player role (activities=%v, active_roles=%v, pairing_method=%s)", sess.Activate.Activities, sess.ActiveRoles, pairingMethodName(sess.Activate.Pairing))
		if sess.Activate.Pairing != nil {
			// The server asked to pair: run the selected method now and use
			// the promoted session.
			after, err := r.pairIfRequested(sess, hello)
			if err != nil {
				conn.Close()
				return err
			}
			sess = after
		} else {
			// The server holds the connection with no roles and no pairing.
			// This is normal for servers like Home Assistant that wait for an
			// operator action before driving pairing or activating roles. Stay
			// connected; readLoop reacts to the later server/activate.
			log.Printf("server holding the connection at empty activities; awaiting a pairing or role activation")
		}
	}

	r.stateMu.Lock()
	r.hello = hello
	// When the server asked us to pair right after connect, the pairing flow
	// already promoted cat to long-term (finishPairing) — don't clobber it
	// with the pre-pairing category.
	if !r.paired {
		r.cat = cat
	}
	r.stateMu.Unlock()
	r.conn = conn
	r.sess = sess
	r.connected.Store(true)
	log.Printf("session established with %s (roles=%v)", sess.ServerName, sess.ActiveRoles)

	// Start the reader first so server/time replies are drained while the
	// initial clock-sync burst is in flight.
	go r.readLoop()
	go r.decodeLoop()
	go r.clockSyncLoop()
	go r.watchClockSync()

	// Only declare availability once the player role is active. On the
	// empty-activate path that happens later, when readLoop sees a pairing or
	// role activation. Read under stateMu: readLoop may have already applied a
	// later activate to the shared session object.
	r.stateMu.Lock()
	activeRoles := sess.ActiveRoles
	r.stateMu.Unlock()
	if hasRole(activeRoles, "player") {
		r.activatePlayer()
	}
	return nil
}

// activatePlayer finishes the connect once the player role is active: run the
// initial clock-sync burst, then report available and push the player state. It
// is idempotent and may be called either from Connect (initial activate) or
// from readLoop (a later role/pairing activation).
func (r *Receiver) activatePlayer() {
	r.stateMu.Lock()
	if r.available {
		r.stateMu.Unlock()
		return
	}
	r.stateMu.Unlock()

	// Per the spec a player sends its initial client/state only after clock
	// synchronization is established, so the server's stream timestamps are
	// trustworthy from the first chunk. Run bursts until the filter has a
	// sample (bounded), then go available.
	r.syncUntilReady(5 * time.Second)

	r.stateMu.Lock()
	r.available = true
	r.volume = 100
	r.stateMu.Unlock()
	if err := r.sendPlayerState(); err != nil {
		log.Printf("send player state: %v", err)
	}
}

// defaultResolvePSK is the handshake PSK selector used when
// Config.ResolvePSK is nil. It honours the persistent pairing store,
// recording which category matched so the session layer can judge the activate
// at the right trust level (long-term when a record matched; Sentinel
// otherwise, which lets an unpaired server open a dynamic pairing-code
// session). A miss returns false and the handshake falls back to the Sentinel
// PSK.
func (r *Receiver) defaultResolvePSK(pskID string, _ secure.PSKCategoryCode) (secure.PSK, bool) {
	r.stateMu.Lock()
	r.matchedCat = session.PSKSentinel
	r.stateMu.Unlock()
	if r.config.Store != nil {
		if psk, ok := r.config.Store.LookupPSK(pskID); ok {
			r.stateMu.Lock()
			r.matchedCat = session.PSKLongTerm
			r.stateMu.Unlock()
			return psk, true
		}
	}
	return secure.PSK{}, false
}

// pairIfRequested completes pairing when the server's initial activate
// declared a pairing activity (which, per the spec, empties active_roles). It
// runs the dynamic pairing-code flow, persists the resulting long-term PSK,
// and returns the promoted session — which carries the player role. When the
// activate did not declare pairing, the missing player role is reported as a
// normal connect error.
func (r *Receiver) pairIfRequested(sess *session.ClientSession, hello session.ClientHello) (*session.ClientSession, error) {
	if sess.Activate.Pairing == nil {
		// The server granted no roles and did not ask to pair. Include what it
		// declared so an unexpected server behaviour (e.g. a server that wants
		// a pairing method we no longer advertise) is diagnosable.
		return nil, fmt.Errorf("server activated no player role and did not request pairing (activities=%v, active_roles=%v); torque supports only the dynamic_pairing_code method and requires pairing", sess.Activate.Activities, sess.ActiveRoles)
	}
	if r.config.Store == nil {
		return nil, fmt.Errorf("server requested pairing (method=%s) but no Store is configured", sess.Activate.Pairing.Method)
	}
	if sess.Activate.Pairing.Method != session.PairMethodDynamicPairingCode {
		return nil, fmt.Errorf("server requested %q pairing; only dynamic_pairing_code is supported (this server may need a pairing method that was removed)", sess.Activate.Pairing.Method)
	}
	return r.pairPIN(sess, hello, r.nextPairingIndex())
}

// pairPIN runs the dynamic pairing-code (PIN) flow, which authenticates the
// connection without a token (the code is the shared secret): it derives and
// surfaces a code for the operator to enter on the server, then mints and
// persists a long-term PSK.
func (r *Receiver) pairPIN(sess *session.ClientSession, hello session.ClientHello, pairingIndex int) (*session.ClientSession, error) {
	// The pairing flow reads the connection directly (it runs on the single
	// readLoop goroutine). Mark it active so clockSyncLoop withholds bursts
	// that would otherwise inject server/time frames into the pairing stream.
	r.pairingActive.Store(true)
	defer r.pairingActive.Store(false)

	p := sess.Activate.Pairing
	format := ""
	if p.Format != nil {
		format = string(*p.Format)
	}
	emit := r.config.PairingCodeEmitted
	if emit == nil {
		emit = func(code string) {
			log.Printf("pairing code: %s (enter it on the server within the window)", code)
		}
	}
	cfg := session.PINPairingClientConfig{
		Method:         string(session.PairMethodDynamicPairingCode),
		PairingIndex:   uint32(pairingIndex),
		Format:         format,
		EmitCode:       emit,
		NewPSK:         func() (secure.PSK, error) { return secure.NewPSK() },
		Persist:        func(_ string, psk secure.PSK) error { return r.config.Store.PutRecord(psk) },
		ResultingHello: hello,
	}
	after, err := sess.RunPINPairing(cfg)
	if err != nil {
		return nil, fmt.Errorf("%s pairing flow: %w", p.Method, err)
	}
	return r.finishPairing(after)
}

// finishPairing verifies the promoted session activated the player role and
// records that this connection paired.
func (r *Receiver) finishPairing(after *session.ClientSession) (*session.ClientSession, error) {
	if !hasRole(after.ActiveRoles, "player") {
		return nil, fmt.Errorf("post-pairing session did not activate the player role (active_roles=%v)", after.ActiveRoles)
	}
	r.stateMu.Lock()
	r.paired = true
	r.cat = session.PSKLongTerm
	r.stateMu.Unlock()
	log.Printf("paired with %s", after.ServerName)
	return after, nil
}

// nextPairingIndex consumes and returns the pairing index for the next pairing
// activation. The first pairing activate since the handshake (even when not the
// first overall activate) gets index 1; each subsequent pairing activate takes
// the next value, matching the server's own per-handshake count.
func (r *Receiver) nextPairingIndex() int {
	r.stateMu.Lock()
	r.pairingIndex++
	idx := r.pairingIndex
	r.stateMu.Unlock()
	return idx
}

// syncUntilReady runs clock-sync bursts until the time filter has produced at
// least one sample, or timeout elapses (a silent server just means the first
// chunks may be dropped; playback still begins).
func (r *Receiver) syncUntilReady(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		r.runTimeSyncBurst(TimeSyncBurstSize)
		if rtt, _ := r.clockSync.GetStats(); rtt > 0 {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("initial clock sync timed out; streaming with an unsynced filter")
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// buildHello assembles the client/hello for this connection.
func (r *Receiver) buildHello() (session.ClientHello, session.PSKCategory) {
	formats := buildFormats(r.config.PreferredCodec, r.config.MaxSampleRate, r.config.MaxBitDepth)
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
		SupportedPairMethods: session.SupportedPairMethods{
			string(session.PairMethodDynamicPairingCode): {
				// Per spec a dynamic_pairing_code descriptor is only
				// recognized when it declares both out_channels and formats.
				// torque conveys the pairing code on screen (display) for the
				// operator to enter; it emits it as decimal digits.
				OutChannels: []string{"display"},
				Formats:     []string{string(session.PairingFormatDigits)},
			},
		},
		UnpairedAccess: session.UnpairedAccess{Enabled: r.config.UnpairedAccess},
	}
	cat := r.config.PSKCategory
	r.stateMu.Lock()
	if r.matchedCat != 0 {
		// Prefer the category the handshake resolver actually selected
		// (long-term when a pairing record matched, Sentinel otherwise).
		cat = r.matchedCat
	}
	r.stateMu.Unlock()
	if cat == 0 {
		// Default to long-term trust only when a pairing record matched the
		// handshake; otherwise the sentinel.
		cat = session.PSKSentinel
	}
	return hello, cat
}

func buildFormats(preferredCodec string, maxSampleRate, maxBitDepth int) []session.AudioFormat {
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

// pairingMethodName renders the server's requested pairing method (or "(none)"
// when the activate declared no pairing) for diagnostics.
func pairingMethodName(p *session.Pairing) string {
	if p == nil {
		return "(none)"
	}
	if p.Format != nil {
		return fmt.Sprintf("%s/%s", p.Method, *p.Format)
	}
	return string(p.Method)
}

// SetVolume stores a local volume change and reports it to the server via
// client/state (the server merges it into the group volume). Muted volume
// changes are folded in but the mute bit stays set, per spec.
func (r *Receiver) SetVolume(volume int) {
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
func (r *Receiver) SetMuted(muted bool) {
	r.stateMu.Lock()
	r.muted = muted
	r.stateMu.Unlock()
	_ = r.sendPlayerState()
}

// PlayerCmd constants above; this is the server→player command verbs this
// receiver truthfully handles, advertised in every player state so the server
// enables those controls (spec: volume only applies when "volume" is in
// supported_commands; a player advertising none reads as having no volume
// control, so HA shows 0).
var playerSupportedCommands = []string{
	string(session.PlayerCmdVolume),
	string(session.PlayerCmdMute),
	string(session.PlayerCmdSetOutputDelay),
}

// sendPlayerState reports the player's full state (available, volume, mute,
// timing) — the spec requires `available` on every client/state.
func (r *Receiver) sendPlayerState() error {
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
			OutputDelayMs:     &od,
			MinBufferMs:       ptr(minb),
			Volume:            &vol,
			Muted:             &mut,
			SupportedCommands: playerSupportedCommands,
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
func (r *Receiver) readLoop() {
	defer r.teardown()

	for {
		msgType, payload, err := r.conn.ReadMessage()
		if err != nil {
			log.Printf("read error: %v", err)
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

func (r *Receiver) handleControl(payload []byte) {
	msg, err := session.Parse(payload)
	if err != nil {
		log.Printf("parse control error: %v", err)
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
		log.Printf("group update: %s (%s)", v.GroupName, v.PlaybackState)
	case *session.ServerActivate:
		r.handleServerActivate(v)
	case *session.ServerUnpair:
		log.Printf("server unpairing this client")
		if r.config.OnUnpair != nil {
			r.config.OnUnpair()
		}
	default:
		// Forward tolerance: ignore unrecognized messages.
	}
}

// handleServerActivate processes a server/activate received after the initial
// one: it keeps the live session in sync, reacts to a pairing request the
// server drives later (dynamic pairing-code), and goes available once the
// player role activates. Servers like Home Assistant often hold an unpaired
// connection at empty activities and only afterwards drive pairing or roles.
//
// The spec's admissibility rules apply to every server/activate, including
// re-sent ones, so the activation is adjudicated exactly like the initial one
// before it is applied (goodbye 'pairing_required' / 'unauthorized', or
// pair/abort for a disallowed pairing method).
func (r *Receiver) handleServerActivate(act *session.ServerActivate) {
	r.stateMu.Lock()
	hello := r.hello
	cat := r.cat
	persisted := r.sess.ActiveRoles
	r.stateMu.Unlock()

	// Effective roles this activation yields, applying the spec's rule that
	// when a later activation omits active_roles and the connection is no
	// longer playback-capable, the persisted roles are treated as empty
	// rather than the message rejected.
	roles := persisted
	if act.ActiveRoles != nil {
		roles = *act.ActiveRoles
	} else if !session.PlaybackCapable(cat, act.Activities, hello.UnpairedAccess.Enabled) {
		roles = nil
	}

	methods := make([]string, 0, len(hello.SupportedPairMethods))
	for m := range hello.SupportedPairMethods {
		methods = append(methods, m)
	}
	verdict := session.JudgeActivate(cat, *act, session.ClientAdmissionState{
		UnpairedAccessEnabled: hello.UnpairedAccess.Enabled,
		OfferedPairMethods:    methods,
		ActiveRolesInEffect:   roles,
	})
	if !verdict.OK {
		r.rejectActivate(verdict)
		return
	}

	r.stateMu.Lock()
	r.sess.Activate = *act
	r.sess.ActiveRoles = roles
	r.stateMu.Unlock()

	if act.Pairing != nil {
		// The server now asks us to pair. This temporarily uses the connection
		// for the pairing exchange, then resumes normal reading.
		after, err := r.pairIfRequested(r.sess, hello)
		if err != nil {
			log.Printf("pairing failed after a later activate: %v", err)
			return // keep the connection; the server may drive another attempt
		}
		r.stateMu.Lock()
		r.sess = after
		roles = after.ActiveRoles
		r.stateMu.Unlock()
	}

	if hasRole(roles, "player") {
		// Run activation off the readLoop goroutine: activatePlayer performs
		// the initial clock-sync burst, which needs readLoop to keep draining
		// server/time replies into timeSyncResp. Running it inline here would
		// deadlock the burst (the only reader is busy running it), so every
		// probe would time out. On the initial Connect path activation runs on
		// the caller goroutine, which is fine because that goroutine isn't the
		// reader.
		go r.activatePlayer()
	}
}

// rejectActivate answers an inadmissible server/activate exactly per the
// spec: an unpaired-access (or other) admissibility failure sends
// client/goodbye and closes the connection; a pairing-method failure sends
// pair/abort and leaves the connection open for the server to retry.
func (r *Receiver) rejectActivate(v session.Verdict) {
	if v.Goodbye != "" {
		if err := writeGoodbye(r.conn, v.Goodbye); err != nil {
			log.Printf("send goodbye %s: %v", v.Goodbye, err)
		}
		log.Printf("server/activate rejected: goodbye %s", v.Goodbye)
		_ = r.conn.Close()
		return
	}
	if v.PairAbort != "" {
		raw, err := session.Marshal(&session.PairAbort{Reason: string(v.PairAbort)})
		if err == nil {
			if err := r.conn.WriteMessage(secure.MsgTypeJSON, raw); err != nil {
				log.Printf("send pair/abort: %v", err)
			}
		}
		log.Printf("pairing activation aborted: %s (leaving connection open)", v.PairAbort)
	}
}

func (r *Receiver) handleStreamStart(sp session.StreamStartPlayer) {
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
		r.notify(fmt.Errorf("create decoder for %s: %w", format.Codec, err))
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
	// A new stream generation: chunks queued from the previous stream must not
	// be scheduled into this one.
	r.streamGen++
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
	go r.pumpSchedulerOutput(newCtx, newScheduler)
}

func (r *Receiver) handleStreamClear(clear session.StreamClear) {
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

func (r *Receiver) handleStreamEnd() {
	if r.config.OnStreamEnd != nil {
		r.config.OnStreamEnd()
	}
	r.stopStream()
}

// handleAudioChunk parses the player-audio header and queues the raw chunk for
// decodeLoop, so codec work never stalls network reads or client/time replies.
func (r *Receiver) handleAudioChunk(body []byte) {
	ts, _, audioData, err := playback.ParseAudioChunk(body)
	if err != nil {
		r.notify(fmt.Errorf("malformed audio chunk: %w", err))
		return
	}

	r.streamMu.Lock()
	if r.decoder == nil || r.scheduler == nil {
		r.streamMu.Unlock()
		return
	}
	gen := r.streamGen
	format := r.format
	r.streamMu.Unlock()

	job := decodeJob{StreamGen: gen, Timestamp: ts, Data: audioData, Format: format}
	select {
	case r.decodeCh <- job:
	case <-r.ctx.Done():
	}
}

// decodeLoop is the single FIFO decode worker: preserves chunk ordering and
// keeps all codec CPU work off the read loop.
func (r *Receiver) decodeLoop() {
	for {
		select {
		case job := <-r.decodeCh:
			r.decodeAndSchedule(job)
		case <-r.ctx.Done():
			return
		}
	}
}

// decodeAndSchedule decodes one raw chunk and schedules the result, guarded so
// a chunk from a superseded stream is never injected into the active one.
func (r *Receiver) decodeAndSchedule(job decodeJob) {
	r.streamMu.Lock()
	if job.StreamGen != r.streamGen || r.decoder == nil || r.scheduler == nil {
		r.streamMu.Unlock()
		return
	}
	decoder := r.decoder
	sched := r.scheduler
	r.streamMu.Unlock()

	pcm, err := decoder.Decode(job.Data)
	if err != nil {
		r.notify(fmt.Errorf("decode error: %w", err))
		return
	}
	if len(pcm) == 0 {
		return
	}
	// A stream may have started or stopped while we decoded; reschedule into
	// the scheduler we validated (skip if it has been replaced).
	r.streamMu.Lock()
	if job.StreamGen != r.streamGen || r.scheduler != sched {
		r.streamMu.Unlock()
		return
	}
	r.streamMu.Unlock()
	sched.Schedule(audio.Buffer{Timestamp: job.Timestamp, Samples: pcm, Format: job.Format})
}

func (r *Receiver) handleServerState(st *session.ServerState) {
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
func (r *Receiver) runTimeSyncBurst(size int) {
	// Only one burst at a time (see syncBurstMu). The periodic clockSyncLoop
	// keeps the filter fresh in steady state, so a concurrent caller that loses
	// the lock can safely skip its own burst.
	if !r.syncBurstMu.TryLock() {
		return
	}
	defer r.syncBurstMu.Unlock()

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
			log.Printf("sync marshal: %v", err)
			continue
		}
		if err := r.conn.WriteMessage(secure.MsgTypeJSON, raw); err != nil {
			log.Printf("sync send %d/%d failed: %v", i+1, size, err)
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
			log.Printf("sync sample %d/%d timed out", i+1, size)
		case <-r.ctx.Done():
			return
		}
	}
	if valid == 0 {
		return
	}
	r.clockSync.ProcessSyncResponse(bestT1, bestT2, bestT3, bestT4)
}

func (r *Receiver) clockSyncLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Withhold bursts while a pairing flow owns the connection so its
			// server/time replies don't collide with pairing messages (which
			// are read directly from the stream).
			if !r.pairingActive.Load() {
				r.runTimeSyncBurst(TimeSyncBurstSize)
			}
		case <-r.ctx.Done():
			return
		}
	}
}

// watchClockSync implements graceful degradation on clock-sync loss (same
// semantics as the legacy Receiver).
func (r *Receiver) watchClockSync() {
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

func (r *Receiver) pausePlayback() {
	r.streamMu.Lock()
	sched := r.scheduler
	r.streamMu.Unlock()
	if sched != nil {
		sched.Clear()
	}
	r.syncLost.Store(true)
	r.notify(ErrSyncLost)
}

func (r *Receiver) resumePlayback() {
	r.streamMu.Lock()
	sched := r.scheduler
	r.streamMu.Unlock()
	if sched != nil {
		sched.Clear()
	}
	r.syncLost.Store(false)
	r.notify(ErrSyncRecovered)
}

func (r *Receiver) pumpSchedulerOutput(ctx context.Context, sched *Scheduler) {
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
func (r *Receiver) stopStream() {
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
func (r *Receiver) teardown() {
	r.connected.Store(false)
	r.stopStream()
	r.cancel()
	r.notify(ErrConnectionLost)
}

// notify delivers an error to OnError or the log.
func (r *Receiver) notify(err error) {
	if r.config.OnError != nil {
		r.config.OnError(err)
	} else {
		log.Printf("client: %v", err)
	}
}

// Close shuts the receiver down, free any stream first, then the connection.
func (r *Receiver) Close() error {
	if r.conn != nil {
		_ = writeGoodbye(r.conn, session.GoodbyeShutdown)
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

func writeGoodbye(conn *secure.Conn, reason session.GoodbyeReason) error {
	raw, err := session.Marshal(&session.ClientGoodbye{Reason: reason})
	if err != nil {
		return err
	}
	return conn.WriteMessage(secure.MsgTypeJSON, raw)
}

// SendAvailability re-reports availability (e.g. entering/leaving external
// source mode).
func (r *Receiver) SendAvailability(available bool) error {
	r.stateMu.Lock()
	r.available = available
	r.stateMu.Unlock()
	return r.sendPlayerState()
}

// SendControllerCommand sends a controller command (client/command).
func (r *Receiver) SendControllerCommand(cmd session.ControllerCommand) error {
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

// defaultDecoderFor maps a wire format to its decoder.
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
