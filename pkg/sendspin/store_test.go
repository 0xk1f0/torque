package sendspin

import (
	"path/filepath"
	"testing"

	"github.com/0xk1f0/torque/pkg/protocol/secure"
)

// TestFileStore_Unified verifies one store round-trips both pairing records
// and volume/mute from a single file.
func TestFileStore_Unified(t *testing.T) {
	dir := t.TempDir()
	store := mustStore(t, filepath.Join(dir, "store.json"))

	// No volume yet.
	if _, ok := store.GetPlayerState(); ok {
		t.Fatal("GetPlayerState on fresh store: unexpectedly present")
	}

	// Put a pairing record and a volume in the same store.
	psk := mustPSK(t)
	if err := store.PutRecord(psk); err != nil {
		t.Fatalf("PutRecord: %v", err)
	}
	if err := store.PutPlayerState(VolumeState{Volume: 37, Muted: true}); err != nil {
		t.Fatalf("PutPlayerState: %v", err)
	}

	// Both are visible.
	if got, ok := store.LookupPSK(psk.ID()); !ok || got.ID() != psk.ID() {
		t.Fatalf("LookupPSK after put: ok=%v", ok)
	}
	st, ok := store.GetPlayerState()
	if !ok || st != (VolumeState{Volume: 37, Muted: true}) {
		t.Fatalf("GetPlayerState after put: ok=%v st=%+v", ok, st)
	}

	// A second store instance (as a restarted process creates) sees both from
	// the same file — pairing and volume share one on-disk record.
	reloaded := mustStore(t, store.path)
	if got, ok := reloaded.LookupPSK(psk.ID()); !ok || got.ID() != psk.ID() {
		t.Fatalf("reloaded LookupPSK: ok=%v", ok)
	}
	if st, ok := reloaded.GetPlayerState(); !ok || st != (VolumeState{Volume: 37, Muted: true}) {
		t.Fatalf("reloaded GetPlayerState: ok=%v st=%+v", ok, st)
	}
}

// TestFileStore_MissingFileNoError ensures a store with no backing file
// reports no state instead of erroring, and dedupes PutRecord.
func TestFileStore_MissingFileNoError(t *testing.T) {
	store := mustStore(t, filepath.Join(t.TempDir(), "store.json"))
	if _, ok := store.GetPlayerState(); ok {
		t.Fatal("expected no player state on fresh store")
	}
	psk := mustPSK(t)
	if err := store.PutRecord(psk); err != nil {
		t.Fatal(err)
	}
	// Duplicate is a no-op; there should be exactly one record.
	if err := store.PutRecord(psk); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.LookupPSK(psk.ID()); !ok || got.ID() != psk.ID() {
		t.Fatalf("LookupPSK after duplicate put: ok=%v", ok)
	}
}

// TestPlayer_RestoresVolumeFromStore verifies NewPlayer picks up a saved
// volume/mute from the unified store, and that SetVolume/Mute persist so a
// restart returns at the operator's chosen loudness.
func TestPlayer_RestoresVolumeFromStore(t *testing.T) {
	store := mustStore(t, filepath.Join(t.TempDir(), "store.json"))
	if err := store.PutPlayerState(VolumeState{Volume: 41, Muted: true}); err != nil {
		t.Fatal(err)
	}

	p, err := NewPlayer(Config{Store: store})
	if err != nil {
		t.Fatalf("NewPlayer: %v", err)
	}
	if st := p.Status(); st.Volume != 41 || !st.Muted {
		t.Fatalf("restored state mismatch: %+v, want volume=41 muted=true", st)
	}

	// A change persists to the store.
	if err := p.SetVolume(62); err != nil {
		t.Fatal(err)
	}
	if err := p.Mute(false); err != nil {
		t.Fatal(err)
	}
	if st, ok := store.GetPlayerState(); !ok || st != (VolumeState{Volume: 62, Muted: false}) {
		t.Fatalf("persisted after change: ok=%v st=%+v", ok, st)
	}

	// A brand-new player (as after a restart) restores the persisted value.
	p2, err := NewPlayer(Config{Store: store, Reconnect: ReconnectConfig{Enabled: false}})
	if err != nil {
		t.Fatalf("NewPlayer (restart): %v", err)
	}
	if st := p2.Status(); st.Volume != 62 || st.Muted {
		t.Fatalf("restart restore mismatch: %+v, want volume=62 muted=false", st)
	}
	p.Close()
	p2.Close()
}

// TestPlayer_NoStoreKeepsDefault ensures a player without a store starts at
// 100/off.
func TestPlayer_NoStoreKeepsDefault(t *testing.T) {
	p, err := NewPlayer(Config{Reconnect: ReconnectConfig{Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if st := p.Status(); st.Volume != 100 || st.Muted {
		t.Fatalf("default state mismatch: %+v, want volume=100 muted=false", st)
	}
}

func mustStore(t *testing.T, path string) *FileStore {
	t.Helper()
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore(%s): %v", path, err)
	}
	return s
}

func mustPSK(t *testing.T) secure.PSK {
	t.Helper()
	psk, err := secure.NewPSK()
	if err != nil {
		t.Fatalf("secure.NewPSK: %v", err)
	}
	return psk
}

// TestStore_PairingPSKGenerateOnce verifies the pairing PSK is provisioned
// exactly once (per the spec's long-lived, non-consuming lifecycle), survives
// a reload, and that an operator-provisioned PSK is never clobbered.
func TestStore_PairingPSKGenerateOnce(t *testing.T) {
	path := t.TempDir() + "/store.json"

	s := mustStore(t, path)
	if _, ok := s.PairingPSK(); ok {
		t.Fatal("fresh store must have no pairing PSK")
	}
	p1, err := s.EnsurePairingPSK()
	if err != nil {
		t.Fatalf("EnsurePairingPSK: %v", err)
	}
	p2, err := s.EnsurePairingPSK()
	if err != nil {
		t.Fatalf("EnsurePairingPSK (2nd): %v", err)
	}
	if p1 != p2 {
		t.Error("EnsurePairingPSK must not regenerate an existing pairing PSK")
	}

	// A reload-from-disk store sees the same pairing PSK.
	s2 := mustStore(t, path)
	if p3, ok := s2.PairingPSK(); !ok || p3 != p1 {
		t.Error("pairing PSK did not persist across store reload")
	}

	// An operator-provisioned PSK wins and is never overwritten.
	other := mustPSK(t)
	if err := s2.SetPairingPSKIfAbsent(other); err != nil {
		t.Fatalf("SetPairingPSKIfAbsent: %v", err)
	}
	if p3, _ := s2.PairingPSK(); p3 != p1 {
		t.Error("SetPairingPSKIfAbsent clobbered the existing pairing PSK")
	}
}

// TestStore_DeleteRecord removes exactly the long-term record by psk_id.
func TestStore_DeleteRecord(t *testing.T) {
	s := mustStore(t, t.TempDir()+"/store.json")
	a := mustPSK(t)
	b := mustPSK(t)
	if err := s.PutRecord(a); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRecord(b); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRecord(a.ID()); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if _, ok := s.LookupPSK(a.ID()); ok {
		t.Error("deleted record still resolves")
	}
	if psk, ok := s.LookupPSK(b.ID()); !ok || psk != b {
		t.Error("unrelated record was removed")
	}
}
