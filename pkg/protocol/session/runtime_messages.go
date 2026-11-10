// ABOUTME: Runtime playback control messages — client/state, group/update, and
// ABOUTME: the stream lifecycle (stream/start, stream/end) exchanged post-setup.
package session

// PlaybackState is a group's binary playback state.
type PlaybackState string

const (
	// PlaybackPlaying means the group is streaming audio.
	PlaybackPlaying PlaybackState = "playing"
	// PlaybackStopped means the group is idle.
	PlaybackStopped PlaybackState = "stopped"
)

// Valid reports whether p is a spec-defined playback state.
func (p PlaybackState) Valid() bool { return p == PlaybackPlaying || p == PlaybackStopped }

// ClientState is the client→server client/state message: availability plus an
// optional player sub-object. Per the spec `available` MUST be present in
// every message; the player object carries only the changed fields when it is
// a delta update (the server merges into existing state).
type ClientState struct {
	Available bool               `json:"available"`
	Player    *ClientPlayerState `json:"player,omitempty"`
}

// ClientPlayerState is the player object of client/state: the timing fields the
// server's send-ahead scheduler consumes, volume/mute, the supported-command
// list, and an optional preferred format. Pointers mark absent (unchanged)
// fields in a delta. buffer_capacity is NOT part of this object — it lives in
// player@v1_support in client/hello.
type ClientPlayerState struct {
	OutputDelayMs      *int         `json:"output_delay_ms,omitempty"`
	RequiredLeadTimeMs *int         `json:"required_lead_time_ms,omitempty"`
	MinBufferMs        *int         `json:"min_buffer_ms,omitempty"`
	Volume             *int         `json:"volume,omitempty"`
	Muted              *bool        `json:"muted,omitempty"`
	SupportedCommands  []string     `json:"supported_commands,omitempty"`
	Format             *AudioFormat `json:"format,omitempty"`
}

// GroupUpdate is the server→client group/update message. Per the spec every
// message MUST carry all three fields (full state, not a delta).
type GroupUpdate struct {
	PlaybackState PlaybackState `json:"playback_state"`
	GroupID       string        `json:"group_id"`
	GroupName     string        `json:"group_name"`
}

// StreamStartPlayer is the negotiated player audio format carried in
// stream/start.
type StreamStartPlayer struct {
	Codec       string `json:"codec"`
	Channels    int    `json:"channels"`
	SampleRate  int    `json:"sample_rate"`
	BitDepth    int    `json:"bit_depth"`
	CodecHeader string `json:"codec_header,omitempty"` // standard Base64 (e.g. fLaC+STREAMINFO for flac)
}

// StreamStart is the server→client stream/start message opening a stream.
// ServerTransmitted is the server-clock microsecond timestamp stamped at send.
type StreamStart struct {
	ServerTransmitted int64              `json:"server_transmitted"`
	Player            *StreamStartPlayer `json:"player,omitempty"`
}

// StreamClear instructs clients to clear their buffers for the given roles,
// used for seeks and track jumps. from_timestamp, when present, keeps audio
// strictly before that server-clock time and clears only at-or-after it.
type StreamClear struct {
	ServerTransmitted int64    `json:"server_transmitted"`
	FromTimestamp     *int64   `json:"from_timestamp,omitempty"`
	Roles             []string `json:"roles,omitempty"`
}

// StreamEnd is the server→client stream/end message closing active streams.
// Roles, when present, restricts the end to those role families (all if
// omitted).
type StreamEnd struct {
	Roles []string `json:"roles,omitempty"`
}
