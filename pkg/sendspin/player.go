// ABOUTME: High-level Player — composes Receiver with an audio
// ABOUTME: output backend, volume/mute, transport commands, and automatic
// ABOUTME: reconnect with backoff.
package sendspin

import (
	"context"
	"fmt"
	"log"
	stdsync "sync"
	"time"

	"github.com/0xk1f0/torque/pkg/audio"
	"github.com/0xk1f0/torque/pkg/audio/output"
	"github.com/0xk1f0/torque/pkg/protocol/session"
)

// Player plays audio from a server over the encrypted wire. It
// mirrors the legacy Player's surface: Connect/Play/Pause/Stop/Seek,
// SetVolume/Mute, Status, Close, and configurable reconnect. Config.Output
// selects the playback backend (nil = miniaudio default).
type Player struct {
	config Config
	recv   *Receiver
	out    output.Output

	// store persists client state (pairing + volume/mute) tied to the
	// identity; nil when the config provided none.
	store ClientStore

	state   PlayerState
	stateMu stdsync.Mutex

	ctx    context.Context
	cancel context.CancelFunc

	capsResolved bool
}

// NewPlayer builds a Player.
func NewPlayer(config Config) (*Player, error) {
	if config.BufferMs == 0 {
		config.BufferMs = 500
	}
	if config.Reconnect.Enabled {
		if config.Reconnect.InitialDelay <= 0 {
			config.Reconnect.InitialDelay = 500 * time.Millisecond
		}
		if config.Reconnect.MaxDelay <= 0 {
			config.Reconnect.MaxDelay = 30 * time.Second
		}
		if config.Reconnect.Multiplier <= 1.0 {
			config.Reconnect.Multiplier = 2.0
		}
	}
	ctx, cancel := context.WithCancel(context.Background())

	volume, muted := 100, false
	if config.Store != nil {
		if st, ok := config.Store.GetPlayerState(); ok {
			volume, muted = st.Volume, st.Muted
		}
	}

	return &Player{
		config: config,
		store:  config.Store,
		ctx:    ctx,
		cancel: cancel,
		state: PlayerState{
			State:     "idle",
			Volume:    volume,
			Muted:     muted,
			Connected: false,
		},
	}, nil
}

// Connect dials the server and starts the playback pipeline.
func (p *Player) Connect() error {
	p.ensureCapsResolved()
	cfg := p.config
	cfg.OnStreamStart = p.onStreamStart
	cfg.OnStreamEnd = p.onStreamEnd
	cfg.OnCommand = func(cmd session.PlayerCommand) {
		switch cmd.Command {
		case session.PlayerCmdVolume:
			if cmd.Volume != nil {
				_ = p.SetVolume(*cmd.Volume)
			}
		case session.PlayerCmdMute:
			if cmd.Mute != nil {
				_ = p.Mute(*cmd.Mute)
			}
		case session.PlayerCmdSetOutputDelay:
			// Ignore in the high-level player; OutputDelayMs is static here.
		}
	}
	cfg.Output = nil

	recv, err := NewReceiver(cfg)
	if err != nil {
		return err
	}
	if err := recv.Connect(); err != nil {
		return err
	}
	p.recv = recv
	p.setState(func(s *PlayerState) { s.Connected = true; s.State = "idle" })

	// Propagate the (possibly restored) volume/mute to the freshly connected
	// receiver so the server sees the operator's loudness immediately instead
	// of the receiver's default 100/off until the next command.
	p.applyStateToReceiver()

	go p.consumeAudio(recv)

	if cfg.Reconnect.Enabled {
		go p.runReconnectLoop(recv)
	}
	return nil
}

// ensureCapsResolved caps the advertised formats from the output device on
// first use (as the legacy player does).
func (p *Player) ensureCapsResolved() {
	if p.capsResolved {
		return
	}
	p.capsResolved = true
	if p.config.MaxSampleRate != 0 || p.config.MaxBitDepth != 0 || p.config.Output != nil {
		return
	}
	rate, depth, err := output.QueryDeviceCapabilities("")
	if err != nil || rate == 0 {
		return
	}
	p.config.MaxSampleRate = rate
	p.config.MaxBitDepth = depth
}

// onStreamStart opens the output at the negotiated format.
func (p *Player) onStreamStart(format audio.Format) {
	p.stateMu.Lock()
	if p.out == nil {
		p.out = p.config.Output
		if p.out == nil {
			p.out = output.NewMalgo("")
		}
	}
	out := p.out
	p.stateMu.Unlock()

	if err := out.Open(format.SampleRate, format.Channels, format.BitDepth); err != nil {
		p.notifyError(fmt.Errorf("open output: %w", err))
		return
	}
	p.stateMu.Lock()
	vol, mut := p.state.Volume, p.state.Muted
	p.state.Codec = format.Codec
	p.state.SampleRate = format.SampleRate
	p.state.Channels = format.Channels
	p.state.BitDepth = format.BitDepth
	p.state.State = "playing"
	p.stateMu.Unlock()
	out.SetVolume(vol)
	out.SetMuted(mut)
	p.notifyStateChange()
}

func (p *Player) onStreamEnd() {
	p.setState(func(s *PlayerState) { s.State = "idle" })
}

func (p *Player) consumeAudio(recv *Receiver) {
	for {
		select {
		case buf, ok := <-recv.Output():
			if !ok {
				return
			}
			p.stateMu.Lock()
			out := p.out
			p.stateMu.Unlock()
			if out != nil {
				if err := out.Write(buf.Samples); err != nil {
					p.notifyError(fmt.Errorf("playback error: %w", err))
				}
			}
		case <-recv.Done():
			return
		case <-p.ctx.Done():
			return
		}
	}
}

// runReconnectLoop supervises the active receiver and rebuilds it with
// exponential backoff whenever its Done channel closes.
func (p *Player) runReconnectLoop(initial *Receiver) {
	current := initial
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-current.Done():
		}
		p.setState(func(s *PlayerState) { s.Connected = false; s.State = "reconnecting" })

		next, ok := p.reconnectWithBackoff()
		if !ok {
			return
		}
		current = next
		p.stateMu.Lock()
		p.recv = next
		p.stateMu.Unlock()
		p.setState(func(s *PlayerState) { s.Connected = true })
		p.applyStateToReceiver()
		go p.consumeAudio(next)
	}
}

func (p *Player) reconnectWithBackoff() (*Receiver, bool) {
	cfg := p.config.Reconnect
	delay := cfg.InitialDelay
	attempt := 0
	for {
		attempt++
		if cfg.MaxAttempts > 0 && attempt > cfg.MaxAttempts {
			p.notifyError(fmt.Errorf("reconnect: gave up after %d attempts", cfg.MaxAttempts))
			return nil, false
		}
		jittered := jitter(delay, 0.2)
		log.Printf("reconnect attempt %d in %v", attempt, jittered)
		select {
		case <-p.ctx.Done():
			return nil, false
		case <-time.After(jittered):
		}
		cfg2 := p.config
		cfg2.Output = nil
		if cfg.Rediscover != nil {
			if addr, err := cfg.Rediscover(p.ctx); err == nil && addr != "" {
				cfg2.ServerAddr = addr
			}
		}
		recv, err := NewReceiver(cfg2)
		if err == nil && recv != nil {
			if err = recv.Connect(); err == nil {
				log.Printf("reconnect: connected to %s on attempt %d", cfg2.ServerAddr, attempt)
				return recv, true
			}
		}
		log.Printf("reconnect attempt %d failed: %v", attempt, err)
		delay = time.Duration(float64(delay) * cfg.Multiplier)
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
	}
}

// Play/Pause/Stop send controller commands.
func (p *Player) Play() error  { return p.sendController(session.CmdPlay) }
func (p *Player) Pause() error { return p.sendController(session.CmdPause) }
func (p *Player) Stop() error  { return p.sendController(session.CmdStop) }

// Next / Previous send track commands.
func (p *Player) Next() error     { return p.sendController(session.CmdNext) }
func (p *Player) Previous() error { return p.sendController(session.CmdPrevious) }

// Seek jumps to a position in ms.
func (p *Player) Seek(positionMs int) error {
	return p.sendCtl(session.ControllerCommand{Command: session.CmdSeek, PositionMs: &positionMs})
}

// SeekRelative offsets the position by offsetMs.
func (p *Player) SeekRelative(offsetMs int) error {
	return p.sendCtl(session.ControllerCommand{Command: session.CmdSeekRelative, OffsetMs: &offsetMs})
}

func (p *Player) sendController(cmd session.MediaCommand) error {
	return p.sendCtl(session.ControllerCommand{Command: cmd})
}

func (p *Player) sendCtl(cmd session.ControllerCommand) error {
	p.stateMu.Lock()
	recv := p.recv
	p.stateMu.Unlock()
	if recv == nil {
		return fmt.Errorf("not connected")
	}
	if err := recv.SendControllerCommand(cmd); err != nil {
		return err
	}
	p.setState(func(s *PlayerState) {
		switch cmd.Command {
		case session.CmdPlay:
			s.State = "playing"
		case session.CmdPause:
			s.State = "paused"
		case session.CmdStop:
			s.State = "idle"
		}
	})
	return nil
}

// SetVolume sets the local volume (0-100), persists it, and reports it to the
// server.
func (p *Player) SetVolume(volume int) error {
	if volume < 0 {
		volume = 0
	}
	if volume > 100 {
		volume = 100
	}
	p.stateMu.Lock()
	p.state.Volume = volume
	out := p.out
	recv := p.recv
	muted := p.state.Muted
	p.stateMu.Unlock()
	if out != nil {
		out.SetVolume(volume)
	}
	if recv != nil {
		recv.SetVolume(volume)
	}
	if p.store != nil {
		if err := p.store.PutPlayerState(VolumeState{Volume: volume, Muted: muted}); err != nil {
			p.notifyError(fmt.Errorf("persist volume: %w", err))
		}
	}
	p.notifyStateChange()
	return nil
}

// Mute sets local mute, persists it, and reports it to the server.
func (p *Player) Mute(muted bool) error {
	p.stateMu.Lock()
	p.state.Muted = muted
	out := p.out
	recv := p.recv
	vol := p.state.Volume
	p.stateMu.Unlock()
	if out != nil {
		out.SetMuted(muted)
		if !muted {
			out.SetVolume(vol)
		}
	}
	if recv != nil {
		recv.SetMuted(muted)
	}
	if p.store != nil {
		if err := p.store.PutPlayerState(VolumeState{Volume: vol, Muted: muted}); err != nil {
			p.notifyError(fmt.Errorf("persist mute: %w", err))
		}
	}
	p.notifyStateChange()
	return nil
}

// applyStateToReceiver pushes the current volume/mute into the active receiver
// (if any), so the server is told the right loudness on first connect and after
// every reconnect.
func (p *Player) applyStateToReceiver() {
	p.stateMu.Lock()
	recv := p.recv
	vol, mut := p.state.Volume, p.state.Muted
	p.stateMu.Unlock()
	if recv == nil {
		return
	}
	recv.SetVolume(vol)
	recv.SetMuted(mut)
}

// Status returns the current player state.
func (p *Player) Status() PlayerState {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.state
}

// Close shuts the player down.
func (p *Player) Close() error {
	p.cancel()
	p.stateMu.Lock()
	recv := p.recv
	out := p.out
	p.stateMu.Unlock()
	if recv != nil {
		_ = recv.Close()
	}
	if out != nil {
		_ = out.Close()
	}
	p.setState(func(s *PlayerState) { s.Connected = false; s.State = "idle" })
	return nil
}

func (p *Player) setState(mut func(*PlayerState)) {
	p.stateMu.Lock()
	mut(&p.state)
	p.stateMu.Unlock()
	p.notifyStateChange()
}

func (p *Player) notifyStateChange() {
	p.stateMu.Lock()
	s := p.state
	p.stateMu.Unlock()
	if p.config.OnStateChange != nil {
		p.config.OnStateChange(s)
	}
}

func (p *Player) notifyError(err error) {
	if p.config.OnError != nil {
		p.config.OnError(err)
	} else {
		log.Printf("player: %v", err)
	}
}
