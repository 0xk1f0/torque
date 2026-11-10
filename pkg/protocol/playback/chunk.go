// ABOUTME: Player-audio binary message encoding — the type byte, the 8-byte
// ABOUTME: microsecond timestamp, and the 4-byte send_ahead header per the spec.
package playback

import (
	"encoding/binary"
	"fmt"
)

// Player-audio binary message types. The spec encodes the role in bits 7–2 and
// the slot in bits 1–0; the player role owns types 4–7 (four slots). A group's
// primary audio stream is slot 0.
const (
	// PlayerAudioMsgType is the transport message type for a player-role audio
	// chunk on slot 0. Pass it as the msgType to Conn.WriteMessage.
	PlayerAudioMsgType byte = 4
)

// AudioChunkMessageHeaderSize is the full player-audio message header per the
// spec: 1 type byte + 8 timestamp bytes + 4 send_ahead bytes = 13 bytes. A
// message shorter than this is a protocol error.
const AudioChunkMessageHeaderSize = 13

// AudioChunkBodyHeaderSize is the header carried inside the message body
// (everything but the transport type byte): the 8-byte big-endian microsecond
// playback timestamp plus the 4-byte big-endian send_ahead, = 12 bytes.
const AudioChunkBodyHeaderSize = 12

// sendAheadMax is the largest value the uint32 send_ahead field can carry
// (~71 minutes in microseconds).
const sendAheadMax = 0xffffffff

// clampSendAhead applies the spec's saturation rules: the field reports the
// lead (in µs) the server had in hand at transmission. It MUST be 0 when the
// server transmits at or after the playback timestamp, and 0xffffffff when the
// true lead exceeds what the field can represent. Both saturation values mean
// "no lead was measured" — receivers must not use them as delay samples.
func clampSendAhead(leadUS int64) uint32 {
	if leadUS <= 0 {
		return 0
	}
	if leadUS >= sendAheadMax {
		return sendAheadMax
	}
	return uint32(leadUS)
}

// EncodeAudioChunk builds a player-audio message body: the 8-byte big-endian
// playback timestamp (server clock, microseconds), the 4-byte big-endian
// send_ahead (µs from server transmission to that timestamp, saturated), and
// the codec payload. Send it with Conn.WriteMessage(PlayerAudioMsgType, body).
func EncodeAudioChunk(timestampUS, transmitUS int64, audio []byte) []byte {
	body := make([]byte, AudioChunkBodyHeaderSize+len(audio))
	binary.BigEndian.PutUint64(body[:8], uint64(timestampUS))
	sendAhead := clampSendAhead(timestampUS - transmitUS)
	binary.BigEndian.PutUint32(body[8:AudioChunkBodyHeaderSize], sendAhead)
	copy(body[AudioChunkBodyHeaderSize:], audio)
	return body
}

// ParseAudioChunk splits a player-audio message body into its playback
// timestamp, send_ahead, and codec payload. It errors when the body is shorter
// than the 12-byte body header (the transport layer enforces the full 13-byte
// message minimum). Callers MUST treat a send_ahead of 0 or 0xffffffff as
// "no lead measured", not a real lead.
func ParseAudioChunk(body []byte) (timestampUS int64, sendAhead uint32, audio []byte, err error) {
	if len(body) < AudioChunkBodyHeaderSize {
		return 0, 0, nil, fmt.Errorf("audio chunk body %d bytes, need at least %d", len(body), AudioChunkBodyHeaderSize)
	}
	return int64(binary.BigEndian.Uint64(body[:8])),
		binary.BigEndian.Uint32(body[8:AudioChunkBodyHeaderSize]),
		body[AudioChunkBodyHeaderSize:], nil
}
