// ABOUTME: Table tests for the server/activate admissibility rules — every
// ABOUTME: row of the spec's PSK-category table plus the verdict ordering.
package session

import "testing"

func acts(a ...Activity) []Activity { return a }

func TestAllowedActivities_SpecTable(t *testing.T) {
	cases := []struct {
		name     string
		psk      PSKCategory
		acts     []Activity
		unpaired bool
		want     bool
	}{
		// Long-term PSK: [] or ['playback']; never pairing.
		{"longterm empty", PSKLongTerm, acts(), false, true},
		{"longterm playback", PSKLongTerm, acts(ActivityPlayback), false, true},
		{"longterm pairing", PSKLongTerm, acts(ActivityPairing), false, false},
		{"longterm pairing+playback", PSKLongTerm, acts(ActivityPairing, ActivityPlayback), false, false},
		// Pairing PSK: [], ['pairing'], ['playback']¹, ['playback','pairing']¹.
		{"pairingpsk empty", PSKPairing, acts(), false, true},
		{"pairingpsk pairing", PSKPairing, acts(ActivityPairing), false, true},
		{"pairingpsk playback", PSKPairing, acts(ActivityPlayback), false, false},
		{"pairingpsk playback unpaired-on", PSKPairing, acts(ActivityPlayback), true, true},
		{"pairingpsk pairing+playback", PSKPairing, acts(ActivityPairing, ActivityPlayback), false, false},
		{"pairingpsk pairing+playback unpaired-on", PSKPairing, acts(ActivityPairing, ActivityPlayback), true, true},
		// Sentinel: same four as pairing PSK.
		{"sentinel empty", PSKSentinel, acts(), false, true},
		{"sentinel pairing", PSKSentinel, acts(ActivityPairing), false, true},
		{"sentinel playback unpaired-off", PSKSentinel, acts(ActivityPlayback), false, false},
		{"sentinel playback unpaired-on", PSKSentinel, acts(ActivityPlayback), true, true},
		{"sentinel pairing+playback unpaired-on", PSKSentinel, acts(ActivityPairing, ActivityPlayback), true, true},
		// Structural rules.
		{"duplicate activity", PSKLongTerm, acts(ActivityPlayback, ActivityPlayback), false, false},
		{"unknown activity", PSKLongTerm, []Activity{"streaming"}, false, false},
		{"management is not a spec activity", PSKSentinel, []Activity{ActivityPlayback, "management"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AllowedActivities(tc.psk, tc.acts, tc.unpaired); got != tc.want {
				t.Errorf("AllowedActivities(%v, %v, unpaired=%v) = %v, want %v",
					tc.psk, tc.acts, tc.unpaired, got, tc.want)
			}
		})
	}
}

func TestPlaybackCapable(t *testing.T) {
	// Long-term PSK with no playback declared: extending with playback is
	// allowed, so the connection may carry roles even without 'playback'.
	if !PlaybackCapable(PSKLongTerm, acts(), false) {
		t.Error("long-term connection should be playback-capable")
	}
	// Long-term with pairing declared is inadmissible, so not capable.
	if PlaybackCapable(PSKLongTerm, acts(ActivityPairing), false) {
		t.Error("long-term pairing-declared connection must not be playback-capable")
	}
	// Pairing PSK alone is not playback-capable.
	if PlaybackCapable(PSKPairing, acts(ActivityPairing), false) {
		t.Error("pairing-psk connection must not be playback-capable")
	}
	// Sentinel without unpaired access is not playback-capable.
	if PlaybackCapable(PSKSentinel, acts(), false) {
		t.Error("sentinel connection without unpaired access must not be playback-capable")
	}
	if !PlaybackCapable(PSKSentinel, acts(), true) {
		t.Error("sentinel connection with unpaired access should be playback-capable")
	}
}

func roles(r ...string) *[]string { s := append([]string{}, r...); return &s }

func pairing(m PairingMethod) *Pairing { return &Pairing{Method: m} }

func TestJudgeActivate_VerdictRules(t *testing.T) {
	cases := []struct {
		name    string
		psk     PSKCategory
		act     ServerActivate
		state   ClientAdmissionState
		wantOK  bool
		goodbye GoodbyeReason
		abort   PairAbortReason
	}{
		{
			name:   "unpaired playback admitted",
			psk:    PSKSentinel,
			act:    ServerActivate{Activities: acts(ActivityPlayback), ActiveRoles: roles("player@v1")},
			state:  ClientAdmissionState{UnpairedAccessEnabled: true},
			wantOK: true,
		},
		{
			// Rule 1 beats rule 2: enabling unpaired access would admit it.
			name:    "sentinel playback without unpaired access -> pairing_required",
			psk:     PSKSentinel,
			act:     ServerActivate{Activities: acts(ActivityPlayback), ActiveRoles: roles()},
			state:   ClientAdmissionState{UnpairedAccessEnabled: false},
			goodbye: GoodbyePairingRequired,
		},
		{
			// Rule 2: roles on a non-playback-capable connection.
			name:    "roles on pairing-psk connection -> unauthorized",
			psk:     PSKPairing,
			act:     ServerActivate{Activities: acts(ActivityPairing), ActiveRoles: roles("player@v1"), Pairing: pairing(PairMethodPairingPSK)},
			state:   ClientAdmissionState{OfferedPairMethods: []string{"pairing_psk"}},
			goodbye: GoodbyeUnauthorized,
		},
		{
			name:   "pairing-psk pairing with matching method",
			psk:    PSKPairing,
			act:    ServerActivate{Activities: acts(ActivityPairing), ActiveRoles: roles(), Pairing: pairing(PairMethodPairingPSK)},
			state:  ClientAdmissionState{OfferedPairMethods: []string{"pairing_psk"}},
			wantOK: true,
		},
		{
			// Rule 3: pairing_psk method on a non-pairing PSK.
			name:  "sentinel pairing with pairing_psk method -> method_not_supported",
			psk:   PSKSentinel,
			act:   ServerActivate{Activities: acts(ActivityPairing), ActiveRoles: roles(), Pairing: pairing(PairMethodPairingPSK)},
			state: ClientAdmissionState{OfferedPairMethods: []string{"pairing_psk", "static_pairing_code"}},
			abort: PairAbortMethodNotSupported,
		},
		{
			// Rule 3: method the client did not offer.
			name:  "pairing with unoffered method -> method_not_supported",
			psk:   PSKSentinel,
			act:   ServerActivate{Activities: acts(ActivityPairing), ActiveRoles: roles(), Pairing: pairing(PairMethodDynamicPairingCode)},
			state: ClientAdmissionState{OfferedPairMethods: []string{"static_pairing_code"}},
			abort: PairAbortMethodNotSupported,
		},
		{
			// Rule 3: pairing without a pairing object.
			name:  "pairing without pairing object -> method_not_supported",
			psk:   PSKSentinel,
			act:   ServerActivate{Activities: acts(ActivityPairing), ActiveRoles: roles()},
			state: ClientAdmissionState{OfferedPairMethods: []string{"static_pairing_code"}},
			abort: PairAbortMethodNotSupported,
		},
		{
			// Rule 3: dynamic pairing code requires a format.
			name:  "dynamic pairing code without format -> method_not_supported",
			psk:   PSKSentinel,
			act:   ServerActivate{Activities: acts(ActivityPairing), ActiveRoles: roles(), Pairing: pairing(PairMethodDynamicPairingCode)},
			state: ClientAdmissionState{OfferedPairMethods: []string{"dynamic_pairing_code"}},
			abort: PairAbortMethodNotSupported,
		},
		{
			name:   "dynamic pairing code with offered format",
			psk:    PSKSentinel,
			act:    ServerActivate{Activities: acts(ActivityPairing), ActiveRoles: roles(), Pairing: &Pairing{Method: PairMethodDynamicPairingCode, Format: pf(PairingFormatDigits)}},
			state:  ClientAdmissionState{OfferedPairMethods: []string{"dynamic_pairing_code"}},
			wantOK: true,
		},
		{
			// pairing object must be absent without 'pairing'.
			name:    "pairing object without pairing activity -> unauthorized",
			psk:     PSKLongTerm,
			act:     ServerActivate{Activities: acts(ActivityPlayback), ActiveRoles: roles(), Pairing: pairing(PairMethodStaticPairingCode)},
			state:   ClientAdmissionState{},
			goodbye: GoodbyeUnauthorized,
		},
		{
			// Omitted active_roles persists previous roles — still needs
			// playback capability.
			name:    "persisted roles on pairing psk -> unauthorized",
			psk:     PSKPairing,
			act:     ServerActivate{Activities: acts(ActivityPairing), Pairing: pairing(PairMethodPairingPSK)},
			state:   ClientAdmissionState{OfferedPairMethods: []string{"pairing_psk"}, ActiveRolesInEffect: []string{"player@v1"}},
			goodbye: GoodbyeUnauthorized,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := JudgeActivate(tc.psk, tc.act, tc.state)
			if v.OK != tc.wantOK || v.Goodbye != tc.goodbye || v.PairAbort != tc.abort {
				t.Errorf("JudgeActivate = %+v, want ok=%v goodbye=%q abort=%q",
					v, tc.wantOK, tc.goodbye, tc.abort)
			}
		})
	}
}

func pf(f PairingFormat) *PairingFormat { return &f }
