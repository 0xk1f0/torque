# Spec-v2 working state (branch `feat/spec-v2-current`)

**Purpose:** progress ledger for bringing the encrypted spec-v2 stack up to the
current [`Sendspin/spec`](https://github.com/Sendspin/spec) (baseline `c281aeb`,
2026-11-09) and then merging with `feat/protocol-and-stability` into `main`.

This branch is a working branch off `origin/feat/spec-v2` (the July 2026 stack:
Noise `KKpsk2` transport, session model, pairing, send-ahead). It is **not yet
ready to merge to `main`** — the `pkg/sendspin` binaries still run the legacy
cleartext profile, and several interop-critical v2 gaps remain (below). Merging
now would ship a `main` whose binaries speak a protocol older than the spec
while the v2 SDK is still incomplete — worse than today's `main`.

## Done on this branch (committed)

`fix(protocol): interop-core wire conformance (session/playback/server)` —
the session/audio wire fixes from the `docs/SPEC_COMPLIANCE.md` §2.1 audit:

- Audio chunk header is 13 bytes total (`[type][8B ts][4B send_ahead]`) with
  `send_ahead` saturation (`0` / `0xffffffff`); min-length 13 enforced by the
  client's binary parser once a v2 client exists.
- Buffer accounting counts the 13-byte header + payload; completion time is
  `timestamp + duration − output_delay_ms`; reset on `stream/end` and on a
  full `stream/clear`; `from_timestamp` partial clears supported
  (`BufferTracker.Reset` / `DiscardSince`, `playback.Group.ClearBuffers`).
- Field renames to the current spec: `output_delay_ms`,
  `set_output_delay`, `muted` in the player state object.
- `client/state.available` is REQUIRED on every message; the server gates
  `stream/start` on `available: true`.
- `stream/clear` added (server_transmitted, from_timestamp?, roles) with
  `GroupManager.ClearStream` + buffer plumbing.
- `stream/end` no longer sends the non-spec `server_transmitted`.
- `group/update` carries all fields (full state); `group_id` is 43-char
  base64url of 32 bytes.
- `server/activate` carries the `pairing` object `{method, format}`; method
  ids now `dynamic_pairing_code` / `pairing_psk` / `static_pairing_code`;
  the invented `management` activity is gone; the admissibility table matches
  the spec (incl. unpaired-access rows).
- `client/hello`: `trust_level` removed; `supported_pair_methods` is an object
  keyed by method id; `player@v1_support` no longer carries
  `supported_commands`; `server/hello` gains optional `languages`.
- v2 server activates only roles it can serve (`ImplementedRoles` →
  `player@vN`); visualizer/source/artwork/color are declined, so the server no
  longer promises `active_roles` it cannot deliver (also avoids the
  `server/state` MUST violation for metadata/color).
- `stream/start` player object gains `codec_header`.

Full repo green under `-race`.

## Remaining (audit-verified, in priority order)

### 1. Secure transport (INTEROP-CRITICAL — a spec peer rejects these today)
- **`psk_category` in the Noise message-1 payload** (`secure/handshake.go`):
  message 1 must carry `psk_id` *and* `psk_category` (`'lt'|'pr'|'sn'`); the
  client selects the PSK by category, and a `psk_id` held under a different
  category is a lookup miss.
- **Client Sentinel fallback** (`handshake.go:Accept`): on an initial-handshake
  `psk_id` miss the client completes noise message 2 with the Sentinel PSK
  instead of failing.
- **Server credential-mismatch path** (`secure/conn.go`): re-verify noise
  message 2 under the Sentinel when the referenced PSK fails (and wasn't the
  Sentinel itself); a Sentinel-validated message 2 ⇒ authenticated
  credential-mismatch ⇒ session is pairing-only (no role activation, no
  `'playback'` while a record exists). Neither side may remove the record.
- **`server/error` init failures** (`secure` accept path): send
  `server/error` with `unsupported_version`/`unsupported_suite`/`malformed`
  for `client/init` failures, checking `version` before `suite` before other
  fields, then close.
- **Re-handshake discipline** (`secure/rehandshake.go`, `session/pairing.go`,
  `session/pin_pairing.go`): after a post-pairing re-handshake the server MUST
  send `server/activate` as its first new-key message and must NOT re-send
  `server/hello`/`client/hello`; the activation is a "subsequent" one with
  `active_roles` omission semantics.
- **Fragmentation wire format** (`secure/frame.go`): use type `1` with
  `[1][flags][orig_type][data]` / `[1][flags][data]` (bit 1 = first, bit 0 =
  last, reserved bits zero); the current `MsgTypeFragmentMore/End = 2/3`
  format is non-spec. Keep the 65518-byte Noise-payload limit.
- Empty-plaintext / malformed-sequence close rules are already present ✅.

### 2. Pairing (self-consistent but non-spec; per the "minimal pairing
   correctness" scope chosen)
- PIN/Pairing-code derivation (`secure/pinpairing.go`): label
  `"sendspin-pairing-code-derive-v1"`, fixed 6-digit (`uint256_be(digest)
  mod 10^6`) or the 24-byte digest for `qr_code`; drop the negotiated
  variable-length PIN for dynamic codes (static stays fixed 8-digit).
- CPace `sid` (`secure/pinpairing.go`): add the `round` component
  (`sid = "sendspin-pair-pake-v1" || h || pairing_index || round`).
- **`wrapped_nonce_B`** (`session/pairing.go`, `pin_pairing.go`): reveal
  `nonce_B` only as a 48-byte AEAD wrap under
  `K_wrap = SHA-256("sendspin-pair-nonce-wrap-v1" || sid || ISK)`; field is
  `wrapped_nonce_B`.
- **Pairing-PSK flow must send `client/pair-init` then `client/pair-finalize`
  back-to-back** (`session/pairing.go`) — the server must accept that
  sequence. Currently only finalize is sent.
- **`server/unpair`** message + handling (client removes the record, sends
  `client/goodbye` reason `unpaired`, closes; unpaired session ignores).
- **Pairing token (`SP:` v0/v1)** encode/decode; the pairing PSK must be
  exposed as an `SP:` token for out-of-band distribution.
- `pair/abort` reasons → spec vocabulary (`pairing_code_mismatch`, etc.);
  attempt-timeout timer; `client/pair-pending` / `client/pair-retry` / rounds
  (dynamic) and the static pairing window. (Some may be deferred as features,
  but the abort-reason vocabulary and derivation are interop.)
- `supported_pair_methods` descriptors now the map shape ✅ (this branch).

### 3. v2 client (the missing production half)
- No audio-playing client exists on the v2 wire. Build (new package, e.g.
  `pkg/protocol/clientv2` or extend `pkg/sendspin`):
  1. `secure.ClientHandshake` → `session.EstablishClient` (activate rules,
     `available`, role set from `active_roles`).
  2. Clock sync bursts (`client/time`/`server/time`, 8-sample burst, KS).
  3. Parse `stream/start` / `stream/clear` / `stream/end`; decode pcm/opus/flac;
     scheduler with 13-byte-chunk handling + `send_ahead` (ignore saturation
     samples); output via `pkg/audio/output`.
  4. Reconnect/backoff and clock-sync-loss degradation ported alongside the
     stability slice.
  5. Legacy `pkg/protocol/client.go` (cleartext) is used by the current
     binaries — keep frozen until the v2 client replaces it.

### 4. Merge (only after 1–3 are green + reviewed)
- Merge `feat/protocol-and-stability` into `feat/spec-v2-current` (mostly
  additive: stability + legacy-safe protocol changes + docs; resolve pkg/
  overlap in the legacy `pkg/sendspin` + `pkg/protocol/client.go`).
- Then merge `feat/spec-v2-current` → `main` as the spec-v2 hard cutoff. The
  plan's rule stands: no dual-support window; conformance harness
  (`Sendspin/conformance`) must track the new spec and gate the release.
