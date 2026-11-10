// ABOUTME: Pairing token (SP:) encode/decode — version 0 (pairing PSK with
// ABOUTME: client identity) and version 1 (24-byte dynamic pairing code).
package secure

import (
	"encoding/base32"
	"fmt"
	"strings"
)

// Pairing token prefix and version characters.
const (
	// PairingTokenPrefix is the fixed token prefix.
	PairingTokenPrefix = "SP:"
	// PairingTokenVersion0 carries client_key(32) || pairing_psk(32).
	PairingTokenVersion0 byte = '0'
	// PairingTokenVersion1 carries the 24-byte dynamic qr_code.
	PairingTokenVersion1 byte = '1'

	// pairingCodeBytes is the qr_code length for a version-1 token.
	pairingCodeBytes = 24
)

// safeTokenEncoding encodes with the RFC 4648 base32 alphabet (A-Z, 2-7);
// the token transform then transliterates every '2' to '9' so the string
// stays inside the QR alphanumeric set.
var safeTokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// EncodePairingToken builds "SP:<version><body>" from a raw payload: base32
// the payload (no padding), then transliterate every byte '2' to '9'.
func EncodePairingToken(version byte, payload []byte) (string, error) {
	if version != PairingTokenVersion0 && version != PairingTokenVersion1 {
		return "", fmt.Errorf("unknown pairing token version %q", version)
	}
	body := safeTokenEncoding.EncodeToString(payload)
	return PairingTokenPrefix + string(version) + strings.ReplaceAll(body, "2", "9"), nil
}

// DecodePairingToken parses and decrypts a token, returning its version and
// payload. It is lenient with operator input per the spec: trim whitespace,
// upper-case, strip an optional "SP:" prefix, reject an unknown version,
// transliterate '9'→'2', re-pad, and base32-decode.
func DecodePairingToken(s string) (byte, []byte, error) {
	tok := strings.ToUpper(strings.TrimSpace(s))
	if rest, ok := strings.CutPrefix(tok, PairingTokenPrefix); ok {
		tok = rest
	}
	if len(tok) == 0 {
		return 0, nil, fmt.Errorf("empty pairing token")
	}
	version := tok[0]
	if version != PairingTokenVersion0 && version != PairingTokenVersion1 {
		return 0, nil, fmt.Errorf("unknown pairing token version %q", version)
	}
	body := strings.ToUpper(strings.ReplaceAll(tok[1:], "9", "2"))
	payload, err := safeTokenEncoding.DecodeString(body)
	if err != nil {
		return 0, nil, fmt.Errorf("decode pairing token body: %w", err)
	}
	return version, payload, nil
}

// PairingTokenV0Payload assembles the version-0 payload: the client's static
// public key (43-char base64url from its identity) followed by the pairing
// PSK's 32 bytes.
func PairingTokenV0Payload(clientKey []byte, pairingPSK PSK) []byte {
	out := make([]byte, 0, KeySize+KeySize)
	out = append(out, clientKey...)
	out = append(out, pairingPSK[:]...)
	return out
}

// PairingTokenV0 encodes a version-0 pairing PSK token. clientID is the
// client's static public key (its Noise identity), pairingPSK the device's
// long-lived pairing PSK.
func PairingTokenV0(clientID []byte, pairingPSK PSK) (string, error) {
	return EncodePairingToken(PairingTokenVersion0, PairingTokenV0Payload(clientID, pairingPSK))
}

// PairingTokenV1 encodes a version-1 dynamic qr_code token from the 24-byte
// pairing code.
func PairingTokenV1(code24 []byte) (string, error) {
	if len(code24) != pairingCodeBytes {
		return "", fmt.Errorf("qr_code must be %d bytes, got %d", pairingCodeBytes, len(code24))
	}
	return EncodePairingToken(PairingTokenVersion1, code24)
}

// SplitTokenV0 splits a version-0 payload into (client_id, pairing_psk).
func SplitTokenV0(payload []byte) (clientID []byte, psk PSK, err error) {
	if len(payload) != 2*KeySize {
		return nil, PSK{}, fmt.Errorf("version-0 payload is %d bytes, want %d", len(payload), 2*KeySize)
	}
	copy(psk[:], payload[KeySize:])
	return payload[:KeySize], psk, nil
}
