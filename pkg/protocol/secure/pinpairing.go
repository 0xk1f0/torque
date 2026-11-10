// ABOUTME: Deterministic PIN-pairing primitives — PAKE sid, pairing-code
// ABOUTME: derivation (digits/QR), commit, and PSK/nonce wrapping under CPace.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"

	"golang.org/x/crypto/chacha20poly1305"
)

// CPace associated data for the two roles.
var (
	CPaceADServer = []byte("server")
	CPaceADClient = []byte("client")
)

// Spec labels (exact UTF-8 bytes, no NUL, no separator).
var (
	pakeSIDLabel      = []byte("sendspin-pair-pake-v1")
	commitLabel       = []byte("sendspin-pair-commit-v1")
	pairingCodeDerive = []byte("sendspin-pairing-code-derive-v1")
	pskWrapLabel      = []byte("sendspin-pair-psk-wrap-v1")
	nonceWrapLabel    = []byte("sendspin-pair-nonce-wrap-v1")
)

// wrapNonce is the 12-byte all-zero nonce used for PSK/nonce wrapping (the key
// is single-use, so a fixed nonce is safe).
var wrapNonce = make([]byte, 12)

// BuildPakeSID constructs the CPace session id:
// "sendspin-pair-pake-v1" || h || pairing_index || round, where h is the Noise
// handshake hash, pairing_index the number of pairing server/activate messages
// since the last handshake, and round the round within the attempt (1 for the
// first; always 1 in the static flow) — each a big-endian uint32.
func BuildPakeSID(handshakeHash []byte, pairingIndex, round uint32) []byte {
	sid := make([]byte, 0, len(pakeSIDLabel)+len(handshakeHash)+8)
	sid = append(sid, pakeSIDLabel...)
	sid = append(sid, handshakeHash...)
	sid = binary.BigEndian.AppendUint32(sid, pairingIndex)
	sid = binary.BigEndian.AppendUint32(sid, round)
	return sid
}

// CommitB computes the dynamic-code commitment:
// SHA-256("sendspin-pair-commit-v1" || nonce_B).
func CommitB(nonceB []byte) []byte {
	sum := sha256.Sum256(concat(commitLabel, nonceB))
	return sum[:]
}

// pairingCodeDigest computes
// SHA-256("sendspin-pairing-code-derive-v1" || h || nonce_A || nonce_B).
func pairingCodeDigest(handshakeHash, nonceA, nonceB []byte) []byte {
	sum := sha256.Sum256(concat(pairingCodeDerive, handshakeHash, nonceA, nonceB))
	return sum[:]
}

// DerivePairingDigits returns the 6-digit dynamic pairing code:
// uint256_be(digest) mod 10^6, zero-padded to exactly 6 ASCII digits. The
// pairing code bytes fed into CPace as PRS are these digits' UTF-8 bytes.
func DerivePairingDigits(handshakeHash, nonceA, nonceB []byte) string {
	digest := new(big.Int).SetBytes(pairingCodeDigest(handshakeHash, nonceA, nonceB))
	modulus := big.NewInt(1_000_000)
	code := new(big.Int).Mod(digest, modulus)
	return fmt.Sprintf("%06d", code.Uint64())
}

// DerivePairingQR returns the pairing code for the qr_code emission format:
// the first 24 bytes of the digest, used verbatim as the CPace PRS.
func DerivePairingQR(handshakeHash, nonceA, nonceB []byte) []byte {
	d := pairingCodeDigest(handshakeHash, nonceA, nonceB)
	return d[:24]
}

// wrapAEAD builds the AEAD for PSK wrapping under the given suite and key.
func wrapAEAD(suite Suite, key []byte) (cipher.AEAD, error) {
	switch suite {
	case SuiteAESGCM:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	case SuiteChaChaPoly:
		return chacha20poly1305.New(key)
	default:
		return nil, fmt.Errorf("unknown suite %q for psk wrapping", string(suite))
	}
}

// wrapKey derives K_wrap = SHA-256(label || sid || ISK) for the given field.
func wrapKey(label, sid, isk []byte) []byte {
	sum := sha256.Sum256(concat(label, sid, isk))
	return sum[:]
}

// WrapPSK seals psk under K_wrap (pskWrapLabel) with the session suite's
// AEAD, a 12-byte zero nonce, and empty associated data. Returns the 48-byte
// ciphertext-plus-tag.
func WrapPSK(sid, isk []byte, psk PSK, suite Suite) ([]byte, error) {
	aead, err := wrapAEAD(suite, wrapKey(pskWrapLabel, sid, isk))
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, wrapNonce, psk[:], nil), nil
}

// UnwrapPSK opens a wrapped PSK produced by WrapPSK.
func UnwrapPSK(sid, isk, wrapped []byte, suite Suite) (PSK, error) {
	plain, err := openWrapped(wrapKey(pskWrapLabel, sid, isk), wrapped, suite)
	if err != nil {
		return PSK{}, err
	}
	if len(plain) != KeySize {
		return PSK{}, fmt.Errorf("unwrapped psk is %d bytes, want %d", len(plain), KeySize)
	}
	var p PSK
	copy(p[:], plain)
	return p, nil
}

// WrapNonceB seals the client's nonce_B under K_wrap (nonceWrapLabel) with the
// session suite's AEAD, a 12-byte zero nonce, empty AD — the spec's
// wrapped_nonce_B in client/pair-confirm. Returns the 48-byte
// ciphertext-plus-tag.
func WrapNonceB(sid, isk []byte, nonceB []byte, suite Suite) ([]byte, error) {
	if len(nonceB) != 32 {
		return nil, fmt.Errorf("nonce_B must be 32 bytes, got %d", len(nonceB))
	}
	aead, err := wrapAEAD(suite, wrapKey(nonceWrapLabel, sid, isk))
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, wrapNonce, nonceB, nil), nil
}

// UnwrapNonceB opens a wrapped_nonce_B produced by WrapNonceB.
func UnwrapNonceB(sid, isk, wrapped []byte, suite Suite) ([]byte, error) {
	plain, err := openWrapped(wrapKey(nonceWrapLabel, sid, isk), wrapped, suite)
	if err != nil {
		return nil, err
	}
	if len(plain) != 32 {
		return nil, fmt.Errorf("unwrapped nonce_B is %d bytes, want 32", len(plain))
	}
	return plain, nil
}

// openWrapped decrypts a wrapped value under the given key.
func openWrapped(key, wrapped []byte, suite Suite) ([]byte, error) {
	aead, err := wrapAEAD(suite, key)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, wrapNonce, wrapped, nil)
	if err != nil {
		return nil, fmt.Errorf("unwrap value: %w", err)
	}
	return plain, nil
}
