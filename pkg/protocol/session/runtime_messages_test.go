// ABOUTME: Round-trip tests for the runtime playback control messages through
// ABOUTME: the shared Marshal/Parse registry.
package session

import "testing"

func TestRuntimeMessages_RoundTrip(t *testing.T) {
	vol := 80
	mute := false
	od, lead, minb := 10, 200, 150
	cases := []any{
		&ClientState{
			Available: true,
			Player: &ClientPlayerState{
				OutputDelayMs: &od, RequiredLeadTimeMs: &lead, MinBufferMs: &minb,
				Volume: &vol, Muted: &mute,
				SupportedCommands: []string{"set_output_delay"},
			},
		},
		&GroupUpdate{PlaybackState: PlaybackPlaying, GroupID: "g1", GroupName: "Kitchen"},
		&StreamStart{ServerTransmitted: 123456, Player: &StreamStartPlayer{Codec: "opus", Channels: 2, SampleRate: 48000, BitDepth: 16}},
		&StreamClear{ServerTransmitted: 456, FromTimestamp: nil, Roles: []string{"player"}},
		&StreamEnd{Roles: []string{"player"}},
	}
	for _, want := range cases {
		raw, err := Marshal(want)
		if err != nil {
			t.Fatalf("marshal %T: %v", want, err)
		}
		got, err := Parse(raw)
		if err != nil {
			t.Fatalf("parse %T: %v", want, err)
		}
		if _, ok := got.(UnknownMessage); ok {
			t.Fatalf("%T parsed as UnknownMessage — not registered", want)
		}
	}
}

func TestClientState_AlwaysCarriesAvailable(t *testing.T) {
	// Per the spec, every client/state carries `available`. A delta with a
	// player update must still serialize it.
	minb := 300
	raw, err := Marshal(&ClientState{Available: true, Player: &ClientPlayerState{MinBufferMs: &minb}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := got.(*ClientState)
	if !ok {
		t.Fatalf("parsed %T, want *ClientState", got)
	}
	if !st.Available {
		t.Error("available must round-trip as true on every client/state")
	}
	if st.Player == nil || st.Player.MinBufferMs == nil || *st.Player.MinBufferMs != 300 {
		t.Errorf("min_buffer_ms not round-tripped: %+v", st.Player)
	}
	if st.Player.OutputDelayMs != nil || st.Player.Volume != nil || st.Player.Muted != nil {
		t.Error("absent player fields should be nil after a delta round-trip")
	}
}

func TestStreamClear_FromTimestampRoundTrip(t *testing.T) {
	from := int64(123_000)
	raw, err := Marshal(&StreamClear{ServerTransmitted: 100, FromTimestamp: &from, Roles: []string{"player"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	sc, ok := got.(*StreamClear)
	if !ok {
		t.Fatalf("parsed %T, want *StreamClear", got)
	}
	if sc.FromTimestamp == nil || *sc.FromTimestamp != from {
		t.Errorf("from_timestamp = %v, want %d", sc.FromTimestamp, from)
	}
}

func TestPlaybackState_Valid(t *testing.T) {
	if !PlaybackPlaying.Valid() || !PlaybackStopped.Valid() {
		t.Error("playing/stopped should be valid")
	}
	if PlaybackState("paused").Valid() {
		t.Error("paused is not a spec playback state")
	}
}
