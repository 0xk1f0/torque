// ABOUTME: Transport framing — binary message-type conventions and the
// ABOUTME: fragmentation of messages exceeding a single Noise frame.
package secure

import "fmt"

// Binary message types owned by the transport layer. Role-specific types
// (player 4-7, artwork 8-11, ...) live with their roles.
const (
	// MsgTypeJSON marks a frame whose payload is a UTF-8 JSON message body.
	MsgTypeJSON byte = 0
	// MsgTypeFragment is the fragmentation message type (spec: type 1).
	MsgTypeFragment byte = 1
)

// Fragment flag bits (inside the flags byte of a fragment frame).
const (
	// fragFlagLast set on the final fragment of a message.
	fragFlagLast byte = 0x01
	// fragFlagFirst set on the first fragment of a message.
	fragFlagFirst byte = 0x02
	// fragFlagsReserved are the reserved flag bits; they MUST be zero.
	fragFlagsReserved byte = 0xFC
)

// Fragment payload capacities per frame: every frame's plaintext (including
// its type byte) must fit MaxTransportPlaintext.
const (
	// maxSingleFramePayload is the largest payload that may travel
	// unfragmented: type byte + payload ≤ MaxTransportPlaintext. This works
	// out to the spec's 65 535 − 16 − 1 = 65 518 bytes.
	maxSingleFramePayload = MaxTransportPlaintext - 1
	// maxOpeningFragmentData accounts for the [type][flags][orig_type] prefix.
	maxOpeningFragmentData = MaxTransportPlaintext - 3
	// maxFragmentData accounts for the [type][flags] prefix.
	maxFragmentData = MaxTransportPlaintext - 2
)

// Fragment splits a message (type + payload) into transport-frame plaintexts.
// A message that fits a single frame is returned as one [type][payload] frame
// (the spec forbids fragmenting messages that fit). Larger messages become
// first/continuation/last fragment frames per the spec wire format:
//
//	first fragment       [1][flags(0x02)][orig_type][data]
//	continuation     [1][flags(0x00)][data]
//	last fragment        [1][flags(0x01)][data]
func Fragment(msgType byte, payload []byte) [][]byte {
	if len(payload) <= maxSingleFramePayload {
		frame := make([]byte, 0, 1+len(payload))
		frame = append(frame, msgType)
		frame = append(frame, payload...)
		return [][]byte{frame}
	}

	var frames [][]byte

	// First fragment: [1][flags=first][orig_type][data].
	n := min(len(payload), maxOpeningFragmentData)
	first := make([]byte, 0, 3+n)
	first = append(first, MsgTypeFragment, fragFlagFirst, msgType)
	first = append(first, payload[:n]...)
	frames = append(frames, first)
	rest := payload[n:]

	// Continuation fragments (flags 0) while more remains than the last
	// fragment can carry; the remainder here is never empty because
	// payload > maxSingleFramePayload guarantees leftover past the first.
	for len(rest) > maxFragmentData {
		cont := make([]byte, 0, 2+maxFragmentData)
		cont = append(cont, MsgTypeFragment, byte(0))
		cont = append(cont, rest[:maxFragmentData]...)
		frames = append(frames, cont)
		rest = rest[maxFragmentData:]
	}

	// Last fragment: [1][flags=last][data].
	last := make([]byte, 0, 2+len(rest))
	last = append(last, MsgTypeFragment, fragFlagLast)
	last = append(last, rest...)
	frames = append(frames, last)
	return frames
}

// DefaultMaxMessageSize bounds reassembled message size. The spec does not
// set a limit; this guards against a peer streaming fragments forever.
const DefaultMaxMessageSize = 32 << 20 // 32 MiB

// Reassembler is the receive-side fragmentation state machine: one
// reassembly buffer and the in-flight orig_type, per spec. Not safe for
// concurrent use; the connection read loop owns it.
type Reassembler struct {
	maxSize  int
	inFlight bool
	origType byte
	buf      []byte
}

// NewReassembler creates a Reassembler; maxSize ≤ 0 selects
// DefaultMaxMessageSize.
func NewReassembler(maxSize int) *Reassembler {
	if maxSize <= 0 {
		maxSize = DefaultMaxMessageSize
	}
	return &Reassembler{maxSize: maxSize}
}

// Feed processes one decrypted frame plaintext. For unfragmented frames it
// returns the message immediately; for fragment frames it accumulates and
// returns done=false until the last fragment arrives. Any protocol violation
// (malformed sequences, reserved flag bits, an orig_type of 1) returns an
// error, which is terminal for the connection per the spec.
func (r *Reassembler) Feed(frame []byte) (msgType byte, payload []byte, done bool, err error) {
	if len(frame) == 0 {
		// A binary message with an empty AEAD plaintext is a protocol error.
		return 0, nil, false, fmt.Errorf("empty transport frame")
	}
	t, data := frame[0], frame[1:]

	if t != MsgTypeFragment {
		if r.inFlight {
			// Only one fragmented message may be in flight; a non-fragment
			// message arriving mid-fragmentation is a malformed sequence.
			return 0, nil, false, fmt.Errorf("frame type %d received mid-fragmentation", t)
		}
		return t, data, true, nil
	}

	// Fragment frame: [type=1][flags][...].
	if len(data) == 0 {
		return 0, nil, false, fmt.Errorf("fragment frame missing its flags byte")
	}
	flags := data[0]
	if flags&fragFlagsReserved != 0 {
		return 0, nil, false, fmt.Errorf("fragment flags have reserved bits set: %#x", flags)
	}
	first := flags&fragFlagFirst != 0
	last := flags&fragFlagLast != 0

	if first {
		if r.inFlight {
			return 0, nil, false, fmt.Errorf("first fragment while a fragmented message is in flight")
		}
		if len(data) < 2 {
			return 0, nil, false, fmt.Errorf("first fragment missing its orig_type byte")
		}
		origType := data[1]
		if origType == MsgTypeFragment {
			return 0, nil, false, fmt.Errorf("fragment orig_type must not be the fragment type")
		}
		r.origType = origType
		r.inFlight = true
		if err := r.grow(data[2:]); err != nil {
			return 0, nil, false, err
		}
	} else {
		if !r.inFlight {
			return 0, nil, false, fmt.Errorf("fragment with no fragmented message in flight")
		}
		if err := r.grow(data[1:]); err != nil {
			return 0, nil, false, err
		}
	}

	if last {
		msgType, payload = r.origType, r.buf
		r.reset()
		return msgType, payload, true, nil
	}
	return 0, nil, false, nil
}

func (r *Reassembler) grow(data []byte) error {
	if len(r.buf)+len(data) > r.maxSize {
		r.reset()
		return fmt.Errorf("fragmented message exceeds %d bytes", r.maxSize)
	}
	r.buf = append(r.buf, data...)
	return nil
}

func (r *Reassembler) reset() {
	r.inFlight = false
	r.origType = 0
	r.buf = nil
}
