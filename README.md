# spinsync

**spinsync** is a Go implementation of the [Sendspin Protocol](https://github.com/Sendspin/spec)
**client**: a Noise-encrypted player that connects to a Sendspin server and
plays synchronized audio.

`spinsync` is a fork of [`Sendspin/sendspin-go`](https://github.com/Sendspin/sendspin-go),
reworked **client-only** — every server component (the v2 server, the legacy
engine/TUI, discovery, and the legacy player) was removed.

## Highlights

- **Noise-encrypted transport** (`KKpsk2`) — authenticated, confidential
  end-to-end over plain `ws://`.
- **Spec-conformant session** — role negotiation, `server/activate`
  admissibility, `available`-based streaming, metadata and control commands
  (see [Protocol conformance](#protocol-conformance)).
- **Hi-res audio** — 24-bit PCM/FLAC and Opus decoding, microsecond sync via a
  Kalman time filter.
- **Resilient** — clock-sync-loss pause/resume and automatic reconnect.
- **Small** — no TUI/server/discovery deps; stripped binaries ~8 MB.

## Install

Requirements: Go ≥ 1.27, `pkg-config`, `libopus` (and ALSA on Linux).

```bash
make            # builds ./spinsync
make test       # go test ./...
make lint       # golangci-lint
```

Cross-build all Raspberry Pi architectures (+ x86_64):

```bash
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6 \
    --target export --output type=local,dest=dist .
```

Same matrix in CI: `.github/workflows/v2-client.yml`.

## Usage

```bash
spinsync -server 192.168.1.10:8927 --name "Living Room"
```

| Flag | Description |
|---|---|
| `--server` | Server `host:port` (required) |
| `--name` | Friendly name |
| `--identity` | Identity key path (generated on first run, mode 0600) |
| `--buffer-ms` / `--output-delay-ms` | Playback buffer / `output_delay_ms` |
| `--buffer-capacity` | Buffer bytes advertised to the server |
| `--max-sample-rate` / `--max-bit-depth` | Cap advertised formats |
| `--unpaired-access` | Admit unpaired servers (default true) |

Run as a service: `sudo make install-daemon`, then configure
`/etc/default/spinsync`. One-shot Pi install: `scripts/quickstart-pi.sh`.

## Library

```go
id, _ := secure.LoadOrGenerateIdentity("/home/pi/.config/sendspin/identity")

player, _ := sendspin.NewPlayerV2(sendspin.V2Config{
    ServerAddr: "192.168.1.10:8927",
    Identity:   id,
    Name:       "Living Room",
    OnMetadata: func(m sendspin.Metadata) { log.Printf("%s - %s", m.Artist, m.Title) },
    OnError:    func(err error) { log.Printf("client error: %v", err) },
})
player.Connect()
```

`ReceiverV2` exposes the same pipeline without an audio device
(connect → session → clock sync → stream → decode → `<-chan audio.Buffer`).

## Layout

```
pkg/sendspin         ReceiverV2/PlayerV2, scheduler, reconnect, sync-loss
pkg/protocol/secure  Noise KKpsk2 transport, identities, psk, pairing tokens
pkg/protocol/session Client establishment, activate rules, messaging, pairing
pkg/protocol/playback Player-audio chunk parsing (13-byte header, send_ahead)
pkg/audio            Codec decode (pcm/opus/flac), malgo output
pkg/sync             Kalman clock filter
cmd/spinsync         The CLI
```

## Protocol conformance

Audited against [`Sendspin/spec`](https://github.com/Sendspin/spec) at
`c281aeb` (2026-11-09). Implemented: `KKpsk2` client (responder) handshake
with both suites, `psk_category` selection with Sentinel fallback, type-1
fragmentation, `SP:` pairing tokens, `server/activate` admissibility,
`client/state.available` on every message, stream lifecycle incl.
`from_timestamp`, metadata/controller state and player commands, 13-byte
audio chunks with `send_ahead` handling.

Known limitation: the server-side Sentinel credential-mismatch re-verify is a
server concern (not shipped here); the client implements the matching
fallback and exposes pairing-record resolution via `V2Config.ResolvePSK`.

## License

[LICENSE](LICENSE) (Community Specification License 1.0).
