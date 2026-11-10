# CLAUDE.md

Guidance for AI tooling (Claude Code, and similar) working in this repository.

## Project Overview

`sendspin-client` is the Go implementation of the **client** side of the
[Sendspin Protocol](https://github.com/Sendspin/spec): a Noise-encrypted
player that connects to a Sendspin server and plays synchronized audio. It is
a client-only fork of `Sendspin/sendspin-go` — all server components were
removed (the v2 server `pkg/protocol/server`, the legacy engine/TUI, mDNS
discovery, the legacy player, and the server CLI). The Python sibling
[`aiosendspin`](https://github.com/Sendspin/aiosendspin) and the spec at
[`Sendspin/spec`](https://github.com/Sendspin/spec) are authoritative for wire
behavior.

**Note**: if uncertain about how something in Sendspin is supposed to work,
fetch and refer to the [protocol specification](https://github.com/Sendspin/spec).

## Commands

Native deps: `libopus` (build) / `libopus0` (runtime) + ALSA on Linux. The
Makefile, CI, and Docker all build with `GOFLAGS=-tags=nolibopusfile` so
`gopkg.in/hraban/opus.v2`'s `opus.Stream` parts (the only consumer of
`libopusfile`) are skipped. Use `./install-deps.sh` for brew/apt/dnf.

```bash
make                            # Build ./sendspin-client
make test                       # go test ./...
make test-coverage              # -race + HTML coverage report
make lint                       # golangci-lint --timeout=5m
make build-cross                # cross-build amd64/arm64/armv7/armv6 (needs cross toolchains)
go test ./pkg/sendspin -run TestReceiverV2_EndToEndAudio -race -v
```

Pre-commit (`.pre-commit-config.yaml`) runs gofmt, goimports, go-mod-tidy,
golangci-lint, and `go test -race -v ./...`.

## Architecture

Library-first. The public API in `pkg/` is what consumers import.

- **`pkg/sendspin`** (`v2_client.go`, `v2_player.go`, `scheduler.go`,
  `client_types.go`): High-level client. The library splits the player into a
  `ReceiverV2` (connect, handshake, session, clock sync, decode, schedule —
  emits `<-chan audio.Buffer`) and a `PlayerV2` (a thin wrapper composing
  `ReceiverV2` + `pkg/audio/output.Output`). Use `ReceiverV2` directly for
  visualizers, DSP, or custom outputs. Each `ReceiverV2` owns its own
  `*sync.ClockSync`.
- **`pkg/audio`** (`types.go`, `decode/`, `output/`): Core `Format`/`Buffer`
  types and pcm/opus/flac decoders; `output` ships a single `malgo` backend.
- **`pkg/protocol/secure`** (`conn.go`, `handshake.go`, `frame.go`,
  `identity.go`, `init_messages.go`, `psk.go`, `token.go`, `cpace.go`,
  `pinpairing.go`, `rehandshake.go`, `suite.go`): Noise `KKpsk2` transport.
  The client is the responder (`ClientHandshake`, `Responder.Accept`);
  `Conn` is symmetric. The initiator/server handshake and the CPace primitives
  are retained so tests and pairing can stand up a peer — no server product.
- **`pkg/protocol/session`** (`session.go`, `messages.go`, `activity.go`,
  `runtime_messages.go`, `command_messages.go`, `pairing.go`,
  `pin_pairing.go`): Client session establishment (`EstablishClient`),
  activation admissibility (`JudgeActivate`), message registry, and the
  pairing client flows. A compact `EstablishServer` is kept only for tests /
  as a conformance peer.
- **`pkg/protocol/playback`** (`chunk.go`): player-audio binary parsing
  (13-byte header, `send_ahead`).
- **`pkg/sync`** (`clock.go`, `timefilter.go`): `ClockSync` + `TimeFilter`,
  the 2D Kalman filter tracking offset + drift. Do not regress it without
  re-reading `docs/SPEC_COMPLIANCE.md`.

### Wire invariants (interop-critical — do not change without re-auditing)

- 20 ms audio chunks (50/s), microsecond timestamps, 13-byte binary header
  (`type` + `timestamp` + `send_ahead`); `send_ahead` saturates to `0` /
  `0xffffffff`.
- `client/state` carries `available` on every message; the initial
  `client/state` is sent only after clock sync is established.
- `client/hello` role list, `supported_pair_methods` as a map, no
  `trust_level`.

## Testing

- Tests are co-located (`_test.go` next to source).
- Integration tests that need a counterpart use `pkg/sendspin/v2_client_test.go`'s
  minimal encrypted **test peer** (built from `secure.ServerHandshake` +
  `session.EstablishServer`), never a server product.
- Wire-format changes must stay green against the audit in
  `docs/SPEC_COMPLIANCE.md`.

## Code Style

- Go ≥ 1.27. Module path is `github.com/Sendspin/sendspin-go`.
- Every `.go` file starts with two `// ABOUTME:` header lines summarizing its
  purpose. Match this on new files.
- Linting: golangci-lint with gosimple, govet, ineffassign, unused, gofmt,
  goimports, misspell, errcheck enabled; staticcheck intentionally disabled.
- Conventional commits: `type(scope): subject`.

## Configuration & daemon

`cmd/sendspin-client` is flags-only (no YAML). The systemd unit lives at
`dist/systemd/sendspin-client.{service,env}`; `make install-daemon` and the Pi
quickstart (`scripts/quickstart-pi.sh`) install it. The identity key is
persisted via `secure.LoadOrGenerateIdentity` (0600).
