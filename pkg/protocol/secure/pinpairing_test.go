// ABOUTME: Tests for the pairing primitives, anchored to oracle values
// ABOUTME: independently computed from the spec constructions (SHA-256 of the
// ABOUTME: labelled concatenations), plus wrap/unwrap round trips.
package secure

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// Oracle values independently computed from the spec constructions (SHA-256
// of the labelled concatenations). A drift here means incompatibility with
// every conformant peer.
func TestPINPrimitives_OracleValues(t *testing.T) {
	h := make([]byte, 32)
	for i := range h {
		h[i] = byte(i) // 00..1f
	}
	nonceA := bytes.Repeat([]byte{0xAA}, 32)
	nonceB := bytes.Repeat([]byte{0xBB}, 32)

	if got := hex.EncodeToString(CommitB(nonceB)); got != "c31ebe8c0282c9f764f4d67046eabdec64fccc2015bb9d2041ccf6501f35962e" {
		t.Errorf("CommitB = %s", got)
	}
	// SHA-256("sendspin-pairing-code-derive-v1" || h || nonce_A || nonce_B)
	// mod 10^6, zero-padded.
	if got := DerivePairingDigits(h, nonceA, nonceB); got != "268642" {
		t.Errorf("DerivePairingDigits = %s, want 268642", got)
	}
	if got := hex.EncodeToString(DerivePairingQR(h, nonceA, nonceB)); got != "72156fcd6ab30cfe98dbc135847f318bf5e5a9626a7748a8" {
		t.Errorf("DerivePairingQR = %s", got)
	}
	// sid = "sendspin-pair-pake-v1" || h || pairing_index(BE32) || round(BE32).
	wantSID := "73656e647370696e2d706169722d70616b652d7631000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f0000000300000001"
	if got := hex.EncodeToString(BuildPakeSID(h, 3, 1)); got != wantSID {
		t.Errorf("BuildPakeSID = %s", got)
	}
}

func TestDerivePairingDigits_FixedSixDigits(t *testing.T) {
	h := bytes.Repeat([]byte{0x01}, 32)
	nA := bytes.Repeat([]byte{0x02}, 32)
	nB := bytes.Repeat([]byte{0x03}, 32)
	pin := DerivePairingDigits(h, nA, nB)
	if len(pin) != 6 {
		t.Errorf("pairing code length = %d, want 6", len(pin))
	}
	for _, c := range pin {
		if c < '0' || c > '9' {
			t.Errorf("pairing code has non-digit: %q", pin)
		}
	}
	if len(DerivePairingQR(h, nA, nB)) != 24 {
		t.Error("qr_code must be exactly 24 bytes")
	}
}

func TestBuildPakeSID_RoundEncoded(t *testing.T) {
	h := bytes.Repeat([]byte{0x01}, 32)
	sid := BuildPakeSID(h, 7, 1)
	if !strings.HasSuffix(hex.EncodeToString(sid), "0000000700000001") {
		t.Errorf("sid must end with pairing_index||round BE32: %s", hex.EncodeToString(sid))
	}
}

func TestWrapPSK_RoundTripBothSuites(t *testing.T) {
	sid := BuildPakeSID(bytes.Repeat([]byte{0x11}, 32), 1, 1)
	isk := bytes.Repeat([]byte{0xCC}, 64)
	psk, _ := NewPSK()

	for _, suite := range []Suite{SuiteChaChaPoly, SuiteAESGCM} {
		t.Run(string(suite), func(t *testing.T) {
			wrapped, err := WrapPSK(sid, isk, psk, suite)
			if err != nil {
				t.Fatal(err)
			}
			if len(wrapped) != KeySize+16 {
				t.Errorf("wrapped PSK is %d bytes, want %d", len(wrapped), KeySize+16)
			}
			back, err := UnwrapPSK(sid, isk, wrapped, suite)
			if err != nil {
				t.Fatalf("unwrap: %v", err)
			}
			if back != psk {
				t.Error("wrap/unwrap round trip mismatch")
			}
		})
	}
}

func TestWrapNonceB_RoundTrip(t *testing.T) {
	sid := BuildPakeSID(bytes.Repeat([]byte{0x11}, 32), 1, 1)
	isk := bytes.Repeat([]byte{0xCC}, 64)
	nonceB := bytes.Repeat([]byte{0xBB}, 32)

	for _, suite := range []Suite{SuiteChaChaPoly, SuiteAESGCM} {
		wrapped, err := WrapNonceB(sid, isk, nonceB, suite)
		if err != nil {
			t.Fatal(err)
		}
		if len(wrapped) != 32+16 {
			t.Errorf("wrapped nonce_B is %d bytes, want 48", len(wrapped))
		}
		back, err := UnwrapNonceB(sid, isk, wrapped, suite)
		if err != nil {
			t.Fatalf("unwrap: %v", err)
		}
		if !bytes.Equal(back, nonceB) {
			t.Error("wrap/unwrap nonce_B mismatch")
		}
	}
	// Wrong ISK must fail.
	wrapped, _ := WrapNonceB(sid, isk, nonceB, SuiteChaChaPoly)
	if _, err := UnwrapNonceB(sid, bytes.Repeat([]byte{0xDD}, 64), wrapped, SuiteChaChaPoly); err == nil {
		t.Error("unwrap nonce_B with wrong ISK succeeded")
	}
}

func TestWrapPSK_WrongKeyFails(t *testing.T) {
	sid := BuildPakeSID(bytes.Repeat([]byte{0x11}, 32), 1, 1)
	isk := bytes.Repeat([]byte{0xCC}, 64)
	psk, _ := NewPSK()

	wrapped, err := WrapPSK(sid, isk, psk, SuiteChaChaPoly)
	if err != nil {
		t.Fatal(err)
	}
	// Different ISK derives a different K_wrap → AEAD open must fail.
	if _, err := UnwrapPSK(sid, bytes.Repeat([]byte{0xDD}, 64), wrapped, SuiteChaChaPoly); err == nil {
		t.Error("unwrap with wrong ISK succeeded")
	}
	// Tampered ciphertext must fail.
	wrapped[0] ^= 0x01
	if _, err := UnwrapPSK(sid, isk, wrapped, SuiteChaChaPoly); err == nil {
		t.Error("unwrap of tampered ciphertext succeeded")
	}
}

// The wrap key is suite-independent (SHA-256); only the AEAD differs, so a
// value wrapped with ChaChaPoly cannot be opened as AES-GCM.
func TestWrapPSK_SuiteMismatchFails(t *testing.T) {
	sid := BuildPakeSID(bytes.Repeat([]byte{0x11}, 32), 1, 1)
	isk := bytes.Repeat([]byte{0xCC}, 64)
	psk, _ := NewPSK()

	wrapped, _ := WrapPSK(sid, isk, psk, SuiteChaChaPoly)
	if _, err := UnwrapPSK(sid, isk, wrapped, SuiteAESGCM); err == nil {
		t.Error("cross-suite unwrap succeeded")
	}
}

func TestPairingToken_RoundTrip(t *testing.T) {
	clientKey := bytes.Repeat([]byte{0x01}, 32)
	pairingPSK, _ := NewPSK()

	// Version 0: 64-byte payload, must survive the 2→9 transliteration.
	tok, err := PairingTokenV0(clientKey, pairingPSK)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "SP:0") {
		t.Errorf("token prefix = %q, want SP:0", tok)
	}
	v, payload, err := DecodePairingToken(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v != '0' {
		t.Errorf("version = %q, want 0", v)
	}
	gotKey, gotPSK, err := SplitTokenV0(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotKey, clientKey) || gotPSK != pairingPSK {
		t.Error("version-0 payload mismatch")
	}

	// Lenient decode: lowercase + trailing whitespace is tolerated.
	fuzzy, _, err := DecodePairingToken("  " + strings.ToLower(tok) + " ")
	if err != nil {
		t.Errorf("lenient decode failed: %v", err)
	}
	_ = fuzzy

	// Version 1: 24-byte qr code.
	code := bytes.Repeat([]byte{0xAB}, 24)
	tok1, err := PairingTokenV1(code)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok1, "SP:1") {
		t.Errorf("v1 token prefix = %q, want SP:1", tok1)
	}
	v, payload1, err := DecodePairingToken(tok1)
	if err != nil {
		t.Fatal(err)
	}
	if v != '1' || !bytes.Equal(payload1, code) {
		t.Error("version-1 round trip mismatch")
	}

	// Unknown version rejected.
	if _, _, err := DecodePairingToken("SP:9AA"); err == nil {
		t.Error("unknown token version accepted")
	}
}
