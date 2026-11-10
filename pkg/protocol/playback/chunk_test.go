// ABOUTME: Tests for player-audio chunk encoding — round-trip, send_ahead
// ABOUTME: saturation, and short-body errors.
package playback

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncodeParseAudioChunk_RoundTrip(t *testing.T) {
	audio := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	// Transmit 500ms before the playback timestamp → send_ahead = 500_000.
	body := EncodeAudioChunk(1_234_567, 1_234_567-500_000, audio)
	if len(body) != AudioChunkBodyHeaderSize+len(audio) {
		t.Fatalf("body len = %d, want %d", len(body), AudioChunkBodyHeaderSize+len(audio))
	}
	ts, ahead, got, err := ParseAudioChunk(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ts != 1_234_567 {
		t.Errorf("timestamp = %d, want 1234567", ts)
	}
	if ahead != 500_000 {
		t.Errorf("send_ahead = %d, want 500000", ahead)
	}
	if !bytes.Equal(got, audio) {
		t.Errorf("payload = %v, want %v", got, audio)
	}
}

func TestEncodeAudioChunk_EmptyPayload(t *testing.T) {
	body := EncodeAudioChunk(42, 42-1000, nil)
	ts, ahead, got, err := ParseAudioChunk(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ts != 42 || ahead != 1000 || len(got) != 0 {
		t.Errorf("ts=%d ahead=%d payload=%v, want 42/1000/empty", ts, ahead, got)
	}
}

func readSendAhead(t *testing.T, body []byte) uint32 {
	t.Helper()
	if len(body) < AudioChunkBodyHeaderSize {
		t.Fatalf("body too short for header: %d", len(body))
	}
	return binary.BigEndian.Uint32(body[8:AudioChunkBodyHeaderSize])
}

func TestEncodeAudioChunk_SendAheadSaturation(t *testing.T) {
	const ts = 1_000_000_000

	// Transmit after the playback timestamp (negative lead) → 0. Encoding
	// must still produce a full header (covered by readSendAhead).
	readSendAhead(t, EncodeAudioChunk(ts, ts+999, nil))

	// Exact bounds: lead of max → preserved; lead just over → 0xffffffff.
	if got := readSendAhead(t, EncodeAudioChunk(ts, ts-int64(sendAheadMax), nil)); got != sendAheadMax {
		t.Errorf("lead == max encoded %d, want %d", got, sendAheadMax)
	}
	if got := readSendAhead(t, EncodeAudioChunk(ts, ts-int64(sendAheadMax)-1, nil)); got != sendAheadMax {
		t.Errorf("lead > max encoded %d, want %d (saturated)", got, sendAheadMax)
	}

	// Transmit at/after the playback timestamp → 0.
	if got := readSendAhead(t, EncodeAudioChunk(ts, ts, nil)); got != 0 {
		t.Errorf("lead == 0 encoded %d, want 0", got)
	}
	if got := readSendAhead(t, EncodeAudioChunk(ts, ts+2_000, nil)); got != 0 {
		t.Errorf("negative lead encoded %d, want 0", got)
	}

	// Ordinary lead passes through.
	if got := readSendAhead(t, EncodeAudioChunk(ts, ts-500_000, nil)); got != 500_000 {
		t.Errorf("500ms lead encoded %d, want 500000", got)
	}
}

func TestParseAudioChunk_ShortBody(t *testing.T) {
	if _, _, _, err := ParseAudioChunk([]byte{0x00, 0x01, 0x02}); err == nil {
		t.Fatal("expected error for body shorter than header")
	}
}
