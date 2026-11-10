# Sendspin client — protocol conformance status

This repository is the **Sendspin client**: a Noise-encrypted player that
connects to a Sendspin server and plays synchronized audio. It is a
client-only fork of `Sendspin/sendspin-go` — everything server-side
(`pkg/protocol/server`, the legacy engine/TUI, and the legacy player) has been
stripped.

Spec baseline: [`Sendspin/spec`](https://github.com/Sendspin/spec)
@ `c281aeb` (2026-11-09). See `docs/SPEC_COMPLIANCE.md` for the full
message-by-message audit that this code was conformed against.

## Client conformance

Implemented and tested:

- **Transport** (`pkg/protocol/secure`): Noise `KKpsk2` handshake as the
  client/responder (`client/init` → `server/init` → `noise/handshake` × 2),
  both cipher suites, `psk_category`-aware PSK selection with Sentinel
  fallback on an initial-handshake lookup miss, type-1 flag fragmentation,
  identities + `SP:` pairing tokens, and the re-handshake responder.
  The initiator/server handshake and CPace primitives are retained as test
  peers / pairing progress — they are not shipped as a server product.
- **Session** (`pkg/protocol/session`): `EstablishClient`, activation
  admissibility (`JudgeActivate`), `available`-first `client/state`, stream
  lifecycle (`stream/start`/`clear`/`end` incl. `from_timestamp`),
  metadata/controller `server/state`, player `server/command`, `server/unpair`.
- **Client** (`pkg/sendspin`): `ReceiverV2`/`PlayerV2` — clock-sync bursts
  before `available:true`, 13-byte audio chunk parsing, pcm/opus/flac decode,
  scheduling with `output_delay_ms`, graceful degradation on clock-sync loss,
  reconnect/backoff, volume/mute, controller commands.
- **Sync** (`pkg/sync`): two-dimensional Kalman time filter.

## Known limitations

- **Server-side pairing** is out of scope for a client repo; the client
  exposes `V2Config.ResolvePSK` for pairing records and the pairing *client*
  flows, but operator pairing is a server concern.
- **Server-side Sentinel credential-mismatch re-verify** (the counterpart to
  the client's fallback) is a server concern; see `docs/SPEC_COMPLIANCE.md`.

## Shipping

- `cmd/sendspin-client` — the CLI.
- `Dockerfile` / `.github/workflows/v2-client.yml` — multi-arch builds for
  amd64/arm64/armv7/armv6.
