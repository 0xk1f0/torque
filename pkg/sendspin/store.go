// ABOUTME: Client-side persistent store, tied to the client identity. Holds
// everything the client needs to remember across restarts: the long-term PSKs
// minted by dynamic pairing-code pairing (so a paired client reconnects
// without re-pairing) and the player volume/mute (so a restart returns at the
// operator's chosen loudness). One store, one file, in the identity directory.
package sendspin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/0xk1f0/torque/pkg/protocol/secure"
)

// VolumeState is the persisted player volume/mute pair.
type VolumeState struct {
	// Volume is the loudness in the spec's 0-100 range.
	Volume int `json:"volume"`
	// Muted is the independent mute bit (mute does not fold into Volume).
	Muted bool `json:"muted"`
}

// ClientStore persists client state tied to the identity: pairing records, the
// per-client pairing PSK, and player volume/mute. Both Receiver (pairing) and
// Player (volume/mute) consume the same store.
type ClientStore interface {
	// LookupPSK returns the recorded long-term PSK whose psk_id matches
	// pskID, and whether it was found. A psk_id held under a different
	// category is a miss.
	LookupPSK(pskID string) (secure.PSK, bool)
	// PutRecord persists a long-term PSK (deduplicated by psk_id).
	PutRecord(psk secure.PSK) error
	// DeleteRecord removes the recorded long-term PSK identified by pskID.
	DeleteRecord(pskID string) error

	// PairingPSK returns the provisioned per-client pairing PSK and whether
	// one exists.
	PairingPSK() (secure.PSK, bool)
	// EnsurePairingPSK returns the pairing PSK, generating and persisting a
	// fresh CSPRNG one on first use (generate-once, per the spec's pairing-PSK
	// lifecycle: it is long-lived and never consumed by a pairing).
	EnsurePairingPSK() (secure.PSK, error)
	// SetPairingPSKIfAbsent persists an operator-provisioned pairing PSK the
	// first time it is called, leaving any existing pairing PSK untouched.
	SetPairingPSKIfAbsent(psk secure.PSK) error

	// GetPlayerState returns the saved volume/mute and whether one was
	// present. A missing file yields ok=false, not an error.
	GetPlayerState() (VolumeState, bool)
	// PutPlayerState persists the volume/mute.
	PutPlayerState(VolumeState) error
}

// FileStore is a JSON-on-disk ClientStore backed by a single 0600 file. Safe
// for concurrent use; the file is created lazily on the first write, and a
// missing or corrupt file degrades gracefully (empty store on load).
type FileStore struct {
	path string
	mu   sync.Mutex
}

type persistedStore struct {
	Pairings   []string     `json:"pairings,omitempty"` // PSK strings for secure.ParsePSK
	PairingPSK *string      `json:"pairing_psk,omitempty"`
	Player     *VolumeState `json:"player,omitempty"`
}

// NewFileStore returns a store backed by path. The file is read lazily (and
// created on the first write), so a fresh identity needs no pre-existing file.
func NewFileStore(path string) (*FileStore, error) {
	return &FileStore{path: path}, nil
}

func (s *FileStore) save(p *persistedStore) error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o600)
}

// LookupPSK returns the recorded long-term PSK whose psk_id matches pskID.
func (s *FileStore) LookupPSK(pskID string) (secure.PSK, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return secure.PSK{}, false
	}
	var p persistedStore
	if err := json.Unmarshal(raw, &p); err != nil {
		return secure.PSK{}, false
	}
	for _, v := range p.Pairings {
		psk, err := secure.ParsePSK(v)
		if err != nil {
			continue // skip a corrupt entry rather than failing the whole store
		}
		if psk.ID() == pskID {
			return psk, true
		}
	}
	return secure.PSK{}, false
}

// PutRecord persists a long-term PSK (deduplicated by psk_id).
func (s *FileStore) PutRecord(psk secure.PSK) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.loadPersisted()
	for _, v := range p.Pairings {
		existing, err := secure.ParsePSK(v)
		if err == nil && existing.ID() == psk.ID() {
			return nil // already recorded
		}
	}
	p.Pairings = append(p.Pairings, psk.String())
	return s.save(p)
}

// DeleteRecord removes the recorded long-term PSK identified by pskID.
func (s *FileStore) DeleteRecord(pskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.loadPersisted()
	kept := p.Pairings[:0]
	for _, v := range p.Pairings {
		existing, err := secure.ParsePSK(v)
		if err == nil && existing.ID() == pskID {
			continue
		}
		kept = append(kept, v)
	}
	if len(kept) == len(p.Pairings) {
		return nil // nothing to remove
	}
	p.Pairings = kept
	return s.save(p)
}

// PairingPSK returns the provisioned per-client pairing PSK.
func (s *FileStore) PairingPSK() (secure.PSK, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pairingPSKLocked()
}

// EnsurePairingPSK returns the pairing PSK, generating and persisting a fresh
// CSPRNG one on first use. A previously provisioned PSK is never overwritten.
func (s *FileStore) EnsurePairingPSK() (secure.PSK, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if psk, ok := s.pairingPSKLocked(); ok {
		return psk, nil
	}
	psk, err := secure.NewPSK()
	if err != nil {
		return secure.PSK{}, err
	}
	if err := s.setPairingPSKLocked(psk); err != nil {
		return secure.PSK{}, err
	}
	return psk, nil
}

// SetPairingPSKIfAbsent persists an operator-provisioned pairing PSK the
// first time it is called; a pairing PSK already in the store is untouched.
func (s *FileStore) SetPairingPSKIfAbsent(psk secure.PSK) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pairingPSKLocked(); ok {
		return nil
	}
	return s.setPairingPSKLocked(psk)
}

func (s *FileStore) setPairingPSKLocked(psk secure.PSK) error {
	p, _ := s.loadPersisted()
	enc := psk.String()
	p.PairingPSK = &enc
	return s.save(p)
}

func (s *FileStore) pairingPSKLocked() (secure.PSK, bool) {
	p, _ := s.loadPersisted()
	if p.PairingPSK == nil {
		return secure.PSK{}, false
	}
	psk, err := secure.ParsePSK(*p.PairingPSK)
	if err != nil {
		return secure.PSK{}, false
	}
	return psk, true
}

// GetPlayerState returns the saved volume/mute and whether one was present.
func (s *FileStore) GetPlayerState() (VolumeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.loadPersisted()
	if p.Player == nil {
		return VolumeState{}, false
	}
	return *p.Player, true
}

// PutPlayerState persists the volume/mute.
func (s *FileStore) PutPlayerState(st VolumeState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.loadPersisted()
	p.Player = &st
	return s.save(p)
}

// loadPersisted is the locked-internal loader: parses the on-disk file into a
// persistedStore (empty on missing/corrupt input). Callers hold s.mu.
func (s *FileStore) loadPersisted() (*persistedStore, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &persistedStore{}, nil
	}
	if err != nil {
		return &persistedStore{}, err
	}
	var p persistedStore
	if len(raw) == 0 {
		return &persistedStore{}, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return &persistedStore{}, nil
	}
	return &p, nil
}
