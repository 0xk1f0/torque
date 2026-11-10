# Sendspin Protocol Compliance Audit

**Status:** Working document (validation snapshot)
**Author:** protocol & stability workstream
**Spec baseline:** [`Sendspin/spec`](https://github.com/Sendspin/spec) @
`c281aeb` (2026-11-09, "Define `group_id` as a 43-character base64url value (#340)")
**Method:** manual source audit of this repo against the current spec — every
wire message this checkout speaks was compared field-by-field with the
spec's `messaging.md`, `connection.md`, `pairing.md`, and `roles/*/v1.md`.

This document is the concrete answer to the README's protocol & compatibility
TODO ("validate all message types match the latest Sendspin Protocol spec",
"document protocol extensions or deviations", "explicit protocol-version
negotiation"). It audits **two distinct protocol tracks** that live in this
codebase:

1. **The legacy track** — what `main` ships today (the `pkg/protocol` +
   `pkg/sendspin` implementation Music Assistant and `aiosendspin`-era peers
   speak).
2. **The spec-v2 track** — the unreleased `feat/spec-v2` branch (Noise
   transport, pairing, session model, send-ahead).

The two tracks have different relationships to today's spec, so they are
audited separately. The short version: neither is fully compliant with the
current spec today — the legacy track by design (it predates the current
handshake), and the spec-v2 track because it was pinned to a July 2026 spec
revision and the spec has since moved on.

---

## 1. The legacy track (shipped `main`)

The legacy track implements the pre-Noise protocol family: a
`client/hello`-first plaintext handshake over text frames. It is retained for
backward compatibility with Music Assistant and is what `make` builds today.

### 1.1 Message-by-message audit

Legend: ✅ compliant · ⚠️ partially compliant · ❌ deviates · 🗑 removable.

| Wire message | Current spec shape (baseline) | This repo (`main`) | Status |
|---|---|---|---|
| **mDNS** `_sendspin-server._tcp` / `_sendspin._tcp` + `path` TXT | connection.md | `pkg/discovery/mdns.go` | ✅ |
| **`client/hello`** | `name`, `device_info?` (+`mac_address?`), `supported_roles`, role support objects, `supported_pair_methods`, `unpaired_access` | `messages.go:ClientHello` sends `client_id`, `name`, `version`, `supported_roles`, `device_info`, versioned *and* unversioned support objects | ❌ legacy |
| **`server/hello`** | `{name, languages?, source@v1_support?}` | sends `server_id`, `name`, `version`, `active_roles`, `connection_reason` | ❌ legacy |
| **`client/time` / `server/time`** | `client_transmitted` / `server_received` / `server_transmitted`; bursts ≥ 8 ≤ 10 s apart | `receiver.go` burst of 8 every 10 s; fields match | ✅ |
| **`client/state`** | `available` boolean + role objects | `{player:{state:"synchronized"\|"external_source", volume, muted}}` (no `available`, no timing fields) | ❌ legacy |
| **`server/state`** | `metadata`, `controller`, `color` objects | `metadata` (tristate merge) + `controller` (repeat/shuffle per spec#81); no `color` | ⚠️ |
| **`server/command` player** | `command` ('volume'\|'mute'\|'set_output_delay') + params | `player.{command:"volume"\|"mute", volume, mute}` | ⚠️ missing `set_output_delay` |
| **`stream/start`** | top-level `server_transmitted` + `player` + `artwork` + `visualizer` | `player` + `artwork` objects, no `server_transmitted` | ⚠️ |
| **`stream/clear`** | `server_transmitted`, `from_timestamp?`, `roles` | `roles` only | ⚠️ |
| **`stream/end`** | `roles` (omitted = all) | `roles` | ✅ |
| **`stream/request-format`** | **removed from current spec** (replaced by `client/state.player.format`) | `client.go:SendRequestFormat` still emits it | 🗑 removable |
| **`group/update`** | `playback_state` 'playing'\|'stopped', `group_id` (43-char base64url), `group_name` | sends "playing"/"paused"/"stopped"; free-form `group_id` | ❌ |
| **`client/goodbye`** | 8 reasons (incl. `unauthorized`, `pairing_required`, `concurrent_attempt`, `unpaired`) | sends a 4-reason subset | ⚠️ subset is fine |
| **`client/command` controller** | `controller.{command, ...params}` | bare command string, no params | ⚠️ |
| **Audio chunks (binary type 4)** | `[type 1B][timestamp 8B][send_ahead 4B][paylaod]`, min 13 B | `[type 1B][timestamp 8B][payload]`, min 9 B (`CreateAudioChunk`) | ❌ no `send_ahead` |
| **Artwork (binary types 8–11)** | `[type][timestamp][image]` | `CreateArtworkChunk` | ✅ |
| **Fragmentation (types 2/3)** | 65 518 B max Noise payload; MUST fragment larger | not implemented (plaintext path, no Noise limit) | ⚠️ n/a today |
| **Noise/encryption/pairing** | mandatory (connection.md, pairing.md) | not present | ❌ v2-only |

### 1.2 Extensions made by sendspin-go (spec says MUST NOT send undefined fields)

These ship on the legacy wire today. They exist for Music Assistant
compatibility and are documented here so they are a conscious choice, not an
accident:

- `client/hello.client_id` and a numeric `client/hello.version` — the legacy
  transport used these instead of the Noise identity exchange.
- Unversioned support objects on `client/hello` (`player_support`,
  `metadata_support`, `artwork_support`, `visualizer_support`) alongside the
  versioned `player@v1_support` etc. — MA-era extension.
- `server/hello.connection_reason` ("discovery"|"playback") — legacy only;
  the concept survives as `activities` in `server/activate` on the v2 track.
- `server_id` chosen as a UUID (`server.go`) rather than a base64url pubkey.
- `client/state.player.state` string enum ("synchronized", "external_source",
  "reconnecting" at Player level) instead of the `available` boolean.

### 1.3 Version-negotiation status (README TODO)

- **Role versioning:** the library always advertises versioned roles
  (`player@v1`, `metadata@v1`, `controller@v1`, …) and the server picks the
  first match per role family (`activateRoles`) — aligned with the spec's
  priority-and-activation model. ✅
- **Core version:** the legacy path carries a numeric `version: 1` in
  `client/hello`/`server/hello`. The *current* spec drops numeric
  negotiation entirely: core `version` is an exact-match field only in
  `client/init`/`server/init`, and every other value aborts the handshake
  (`unsupported_version`). So "explicit numeric version negotiation" is not a
  thing in the current spec — role versioning *is* the negotiation, plus a
  hard core-version gate in `client/init`. The README TODO item is answered
  by the current spec: there is intentionally no numeric handshake; the
  spec-v2 track implements the correct `client/init` gate.

### 1.4 Legacy-track verdict

The legacy track is wire-compatible with **Music Assistant's expected
profile**, not with the current spec's transport. That is by design and
documented in `docs/superpowers/plans/2026-07-18-spec-compliance-server.md`
(hard cutover, no dual-support window). No legacy message should be silently
changed to match the current spec — it would break MA. Changes to message
*shapes* belong on the spec-v2 track.

---

## 2. The spec-v2 track (`feat/spec-v2`, unreleased)

The branch implements the current-spec architecture: Noise `KKpsk2`
transport, `server/activate` activities, pairing, send-ahead scheduling,
controller completion. Its plan pinned the spec at
`7c04eb7` (2026-07-18, "Keep the connection open across pairing attempts");
the spec has advanced materially since (398+ commits; baseline `c281aeb`
adds, among others, the 43-char `group_id` rule #340). The following drift
was verified against the baseline.

### 2.1 Verified drift (spec-v2 branch vs today's spec)

| # | Area | Branch today | Current spec | Severity |
|---|---|---|---|---|
| D1 | `client/hello.trust_level` | sent by `pkg/protocol/session/messages.go:ClientHello` (`json:"trust_level"`) | not defined anywhere in the spec anymore — removed since the July pin | High (sending an undefined field violates forward-compat rules) |
| D2 | `server/hello.languages` | absent (`session/messages.go:ServerHello` has only `name`) | `languages?` (BCP 47 list) added; also `source@v1_support?` | Medium |
| D3 | audio chunk wire format | `pkg/protocol/playback/chunk.go:EncodeAudioChunk` emits `[type][8B ts][payload]` (9-byte header) | `[type][8B ts][4B send_ahead][payload]` (13-byte header); min 13 B; buffer accounting on 13-byte headers; `send_ahead` saturation (`0` / `0xffffffff`) | **High — interop-critical** |
| D4 | `client/state` player timing field | `ClientPlayerState.StaticDelayMs` (`static_delay_ms`), commands `set_static_delay` | `output_delay_ms` + `set_output_delay` (rename, spec#332-era) | High |
| D5 | `client/state` player mute field | `ClientPlayerState.Mute` → `json:"mute"` | `muted` (with a d) in the player state object; `mute` is only the command's param | Medium |
| D6 | `group/update.playback_state` | allows "playing"/"paused"/"stopped" | `'playing' | 'stopped'` only — a paused group reports `'stopped'` | Medium |
| D7 | `stream/request-format` | still implemented (`client.go:SendRequestFormat`, `messages.go:StreamRequestFormat`) | removed; replaced by `client/state.player.format` preference | Medium (dead path) |
| D8 | `stream/clear.from_timestamp` | absent | partial-clear field (announcements, changing next-track) — spec requires it for the seek/clear semantics | High |
| D9 | `client/state.player` new required fields | `required_lead_time_ms`, `min_buffer_ms`, `buffer_capacity` present | `output_delay_ms` never optional; `supported_commands` subset changed to `{'volume','mute','set_output_delay'}` | Low–Medium (tied to D4) |
| D10 | `server/hello` `active_roles` removal | `active_roles` handled via `server/activate` (S2) — matches | artifact: the old `connection_reason` gone | ✅ no action |

### 2.2 What the spec-v2 track already gets right (spot-check vs baseline)

- `client/init`→`server/init`→`noise/handshake`×2 prologue-bound handshake,
  both cipher suites, server-as-initiator (`pkg/protocol/secure/`).
- Sentinel PSK + `psk_id`/`psk_category` selection and Sentinel fallback.
- `server/activate` with `activities`/`active_roles`, admissible-activation and
  rejection rules, `pairing` object.
- All eight `client/goodbye` reasons (`session/messages.go:GoodbyeReason`).
- `stream/start`/`stream/end` carry top-level `server_transmitted`.
- `available` boolean drives group moves and solo-group semantics (S7c).
- Disconnect-on-`Stop` for clients and reconnect/backoff on the player side
  (backported to `main` by this workstream — see section 4).

### 2.3 Spec-v2 verdict

The branch is structurally aligned with the current spec but has drifted on
interop-critical wire details (D3, D8 — the audio plane) and field renames
(D4, D5). Because the branch is unreleased and gated on the conformance
harness (see the July plan), these belong as fixes on that branch, not on
`main`: merging `main`'s legacy messages forward is a hard-cutover decision
owned by the spec-v2 track.

---

## 3. Forward & backward compatibility

The current spec's rules and this codebase's behavior:

- **Must ignore unrecognized message types** (valid envelope):
  `protocol.Client.handleJSONMessage` logs and ignores (locked in by
  `TestForwardCompat_IgnoresUnknownJSON`, added by this workstream). ✅
- **Must ignore unrecognized payload fields**: Go's `encoding/json` default ✅
  (same test).
- **Must ignore binary IDs it does not implement**: `handleBinaryMessage`
  drops unknown types (existing `TestHandleBinaryMessage_UnknownTypeLogged`). ✅
- **Must NOT send fields the spec does not define**: violated on the legacy
  track by the MA-compat extensions in §1.2 (documented, deliberate) and on
  the spec-v2 track by D1. Whether any of the current user-facing messages
  send spec-undefined fields on the *v2* wire is exactly what an audit plus
  the conformance harness will catch once the harness tracks the new spec.

---

## 4. Stability work associated with this audit

The stability TODO items landed on `main` as part of this workstream and are
referenced here for completeness:

- **Reconnection + automatic retry** — `Player.Reconnect` (backoff, jitter,
  max-attempts, rediscover) verified end-to-end (`player_reconnect_test.go`).
- **Graceful degradation on clock-sync loss** — `Receiver` pauses playback and
  reports `ErrSyncLost` when the time filter stays `QualityLost` past the
  grace window, resumes on recovery (`receiver_sync_test.go`).
- **Server clean shutdown with live clients** — `Server.Stop` disconnects
  clients (`ServerClient.Disconnect`) so hijacked WebSockets cannot stall
  shutdown and clients observe the drop.
- **Leak/soak + stress + multi-room sync tests** — `stress_test.go`
  (16 concurrent clients; 5-player timestamp-alignment on one 20 ms grid;
  bounded heap/goroutines across connect/stream/close cycles).

---

## 5. Recommended next steps

Prioritized, split by track:

**Spec-v2 track (`feat/spec-v2`), in order of interop risk:**
1. D3 — add `send_ahead` (4-byte BE uint32, saturation `0`/`0xffffffff`) to
   the player audio chunk header, bump the minimum to 13 bytes, and update
   player-role buffer accounting (13-byte headers).
2. D8 — add `stream/clear.from_timestamp` and the partial-clear semantics for
   seeks/announcements.
3. D4/D5 — rename `static_delay_ms`→`output_delay_ms`,
   `set_static_delay`→`set_output_delay`, `mute`→`muted` in the player state
   object; verify against the spec's newest schema.
4. D1 — drop `trust_level` from `client/hello`.
5. D2/D7/D6 — add `server/hello.languages`, remove the
   `stream/request-format` path, clamp `group/update.playback_state` to
   `playing`/`stopped`.
6. Re-run the conformance suite once `Sendspin/conformance` tracks the new
   spec (the release gate from the July plan).

**Legacy track (`main`):** no further wire changes — frozen for MA
compatibility until the hard cutover. `stream/request-format` may be removed
from the *server* without breaking MA (it is client-initiated), but is
harmless to keep.

---

## Appendix A — How the version-negotiation README TODO resolves

- Versioned roles (`role@vN`) with first-match activation and `_`-prefixed
  application roles: implemented on both tracks. ✅
- Numeric core version: the current spec explicitly defines `version: 1` as an
  exact-match field in `client/init`/`server/init` — **not** a negotiation.
  The spec-v2 track implements this. The legacy `client/hello.version` is a
  MA-era leftover on the frozen track.
- "Detecting outdated servers": the current spec defines the SHOULD-track
  mechanism for clients requesting unimplemented role versions — implement on
  the spec-v2 track.
