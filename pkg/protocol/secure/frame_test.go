// ABOUTME: Tests for transport framing and fragmentation: round trips at
// ABOUTME: boundary sizes, frame caps, and receiver protocol violations.
package secure

import (
	"bytes"
	"math/rand"
	"testing"
)

// reassemble feeds frames through a Reassembler and returns the message.
func reassemble(t *testing.T, frames [][]byte) (byte, []byte) {
	t.Helper()
	r := NewReassembler(0)
	for i, f := range frames {
		msgType, payload, done, err := r.Feed(f)
		if err != nil {
			t.Fatalf("Feed frame %d: %v", i, err)
		}
		if done {
			if i != len(frames)-1 {
				t.Fatalf("message completed at frame %d of %d", i+1, len(frames))
			}
			return msgType, payload
		}
	}
	t.Fatal("frames exhausted without completing a message")
	return 0, nil
}

func TestFragment_SmallMessageSingleFrame(t *testing.T) {
	payload := []byte(`{"type":"server/hello","payload":{"name":"x"}}`)
	frames := Fragment(MsgTypeJSON, payload)
	if len(frames) != 1 {
		t.Fatalf("small message produced %d frames, want 1", len(frames))
	}
	if frames[0][0] != MsgTypeJSON {
		t.Errorf("frame type = %d, want %d", frames[0][0], MsgTypeJSON)
	}
	msgType, back := reassemble(t, frames)
	if msgType != MsgTypeJSON || !bytes.Equal(back, payload) {
		t.Error("single-frame round trip mismatch")
	}
}

func TestFragment_BoundarySizes(t *testing.T) {
	cases := []struct {
		size       int
		wantFrames int
	}{
		{0, 1},
		{1, 1},
		{maxSingleFramePayload, 1},     // largest unfragmented message
		{maxSingleFramePayload + 1, 2}, // smallest fragmented message
		{maxOpeningFragmentData + maxFragmentData, 2},
		{maxOpeningFragmentData + maxFragmentData + 1, 3},
	}
	for _, tc := range cases {
		payload := bytes.Repeat([]byte{0xAB}, tc.size)
		frames := Fragment(0x08, payload) // artwork channel 0
		if len(frames) != tc.wantFrames {
			t.Errorf("size %d: %d frames, want %d", tc.size, len(frames), tc.wantFrames)
		}
		for i, f := range frames {
			if len(f) > MaxTransportPlaintext {
				t.Errorf("size %d frame %d: %d bytes exceeds transport cap %d",
					tc.size, i, len(f), MaxTransportPlaintext)
			}
		}
		msgType, back := reassemble(t, frames)
		if msgType != 0x08 || !bytes.Equal(back, payload) {
			t.Errorf("size %d: round trip mismatch", tc.size)
		}
	}
}

func TestFragment_LargeMessageWireShape(t *testing.T) {
	payload := make([]byte, 200_000) // typical artwork image size
	rnd := rand.New(rand.NewSource(1))
	rnd.Read(payload)

	frames := Fragment(0x09, payload)
	if len(frames) < 4 {
		t.Fatalf("200kB message produced only %d frames", len(frames))
	}
	// First fragment: [1][flags=first(0x02)][orig_type][data].
	if frames[0][0] != MsgTypeFragment || frames[0][1] != fragFlagFirst || frames[0][2] != 0x09 {
		t.Errorf("first fragment prefix = [%d %d %d], want [1 2 9]", frames[0][0], frames[0][1], frames[0][2])
	}
	for i := 1; i < len(frames)-1; i++ {
		if frames[i][0] != MsgTypeFragment || frames[i][1]&(fragFlagFirst|fragFlagLast) != 0 {
			t.Errorf("continuation frame %d = [%d %d], want type 1 with no first/last flags", i, frames[i][0], frames[i][1])
		}
	}
	last := frames[len(frames)-1]
	if last[0] != MsgTypeFragment || last[1] != fragFlagLast {
		t.Errorf("last fragment = [%d %d], want [1 1]", last[0], last[1])
	}
	if len(last) < 3 {
		t.Error("last fragment carries no data")
	}

	msgType, back := reassemble(t, frames)
	if msgType != 0x09 || !bytes.Equal(back, payload) {
		t.Error("large round trip mismatch")
	}
}

func TestFragment_ThroughEncryptedSession(t *testing.T) {
	serverSess, clientSess := runHandshake(t, SuiteAESGCM, SentinelPSK(), sentinelSelector)

	payload := make([]byte, 150_000)
	rand.New(rand.NewSource(2)).Read(payload)

	r := NewReassembler(0)
	var gotType byte
	var got []byte
	for _, frame := range Fragment(0x0A, payload) {
		ct, err := serverSess.Encrypt(frame)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		pt, err := clientSess.Decrypt(ct)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		msgType, p, done, err := r.Feed(pt)
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		if done {
			gotType, got = msgType, p
		}
	}
	if gotType != 0x0A || !bytes.Equal(got, payload) {
		t.Error("fragmented message did not survive the encrypted channel")
	}
}

func TestReassembler_Violations(t *testing.T) {
	t.Run("continuation without a first fragment", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment, 0x00, 0x01}); err == nil {
			t.Error("orphan continuation fragment accepted")
		}
	})

	t.Run("non-fragment frame mid-flight", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment, fragFlagFirst, MsgTypeJSON, 0x01}); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := r.Feed([]byte{0x04, 0x00}); err == nil {
			t.Error("interleaved non-fragment frame accepted")
		}
	})

	t.Run("empty frame", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed(nil); err == nil {
			t.Error("empty frame accepted")
		}
	})

	t.Run("first fragment missing orig_type", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment, fragFlagFirst}); err == nil {
			t.Error("first fragment without orig_type accepted")
		}
	})

	t.Run("fragment missing flags byte", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment}); err == nil {
			t.Error("fragment without a flags byte accepted")
		}
	})

	t.Run("reserved flag bits rejected", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment, fragFlagFirst | 0x80, MsgTypeJSON, 0x00}); err == nil {
			t.Error("reserved fragment flag bit accepted")
		}
	})

	t.Run("orig_type of fragment type rejected", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment, fragFlagFirst, MsgTypeFragment, 0x00}); err == nil {
			t.Error("fragment with orig_type 1 accepted")
		}
	})

	t.Run("second first fragment mid-flight", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment, fragFlagFirst, 0x09, 0x01}); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment, fragFlagFirst, 0x0A, 0x01}); err == nil {
			t.Error("second first fragment while one is in flight accepted")
		}
	})

	t.Run("oversized reassembly", func(t *testing.T) {
		r := NewReassembler(16)
		if _, _, _, err := r.Feed(append([]byte{MsgTypeFragment, fragFlagFirst, MsgTypeJSON}, make([]byte, 10)...)); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := r.Feed(append([]byte{MsgTypeFragment, fragFlagLast}, make([]byte, 10)...)); err == nil {
			t.Error("reassembly over the size limit accepted")
		}
	})

	t.Run("usable after violation reset", func(t *testing.T) {
		r := NewReassembler(0)
		if _, _, _, err := r.Feed([]byte{MsgTypeFragment, 0x00, 0x01}); err == nil {
			t.Fatal("orphan continuation fragment accepted")
		}
		msgType, payload, done, err := r.Feed([]byte{MsgTypeJSON, '{', '}'})
		if err != nil || !done || msgType != MsgTypeJSON || string(payload) != "{}" {
			t.Error("reassembler unusable after a violation")
		}
	})
}
