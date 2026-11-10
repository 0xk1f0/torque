# Sendspin Client

A Go implementation of the [Sendspin Protocol](https://github.com/Sendspin/spec)
**client** — a Noise-encrypted player that connects to a Sendspin server and
plays synchronized audio. This is a client-only fork of
`Sendspin/sendspin-go`: every server component (the v2 server, the legacy
engine/TUI, discovery, and the legacy player) has been stripped.

**Highlights:**

- **Encrypted by default** — Noise `KKpsk2` transport, so traffic is
  authenticated and confidential end-to-end over plain `ws://`.
- **Spec-conformant session** — role negotiation, `server/activate`
  admissibility, `available`-based streaming, metadata, and control commands
  audited against the current spec (`docs/SPEC_COMPLIANCE.md`).
- **Hi-res audio** — 24-bit PCM/FLAC and Opus decoding, precise microsecond
  clock sync via a Kalman time filter.
- **Library-first** — use `PlayerV2` for out-of-the-box playback, `ReceiverV2`
  for visualizers/DSP/custom outputs, or the lower-level
  `pkg/protocol/*` and `pkg/sync` APIs.
- **Resilient** — clock-sync-loss pause/resume and automatic reconnect with
  jittered backoff.
- **Small** — the client module drops the TUI, server engine, and discovery
  dependencies; builds are ~8 MB stripped.

## Requirements

- Go ≥ 1.27
- `pkg-config` + libopus (`libopus-dev`; `libopus0` at runtime)
- ALSA dev headers on Linux (`libasound2-dev`)

```bash
# macOS
brew install pkg-config opus
# Ubuntu/Debian
sudo apt-get install pkg-config libopus-dev libasound2-dev
```

## Build

```bash
make            # builds ./sendspin-client
make test       # go test ./...
make lint       # golangci-lint
```

Cross-build every Raspberry Pi architecture (plus x86_64):

```bash
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6 \
    --target export --output type=local,dest=dist .
# or natively: make build-cross   (needs aarch64/gnueabihf cross toolchains)
```

CI builds the same matrix in `.github/workflows/v2-client.yml`.

## Usage

```bash
./sendspin-client -server 192.168.1.10:8927 --name "Living Room"
```

Options:

| Flag | Description |
|---|---|
| `--server` | Server address, `host:port` (required) |
| `--name` | Friendly name (default: `hostname-sendspin-client`) |
| `--identity` | Path to the client identity key (generated on first run, mode 0600) |
| `--buffer-ms` | Playback buffer in milliseconds (default 500) |
| `--output-delay-ms` | `output_delay_ms` reported to the server (0–5000) |
| `--buffer-capacity` | `buffer_capacity` in bytes advertised to the server |
| `--max-sample-rate` / `--max-bit-depth` | Cap the advertised formats (0 = no cap) |
| `--unpaired-access` | Admit unpaired servers (default true) |
| `--version` | Print version and exit |

Run as a systemd service on a Raspberry Pi:

```bash
sudo make install-daemon
sudo systemctl enable --now sendspin-client
# configure via /etc/default/sendspin-client
```

Or use `scripts/quickstart-pi.sh` for a one-shot install.

## Library

### PlayerV2 — play audio

```go
id, _ := secure.LoadOrGenerateIdentity("/home/pi/.config/sendspin/identity")

player, _ := sendspin.NewPlayerV2(sendspin.V2Config{
    ServerAddr: "192.168.1.10:8927",
    Identity:   id,
    Name:       "Living Room",
    Volume:     80,
    OnMetadata: func(m sendspin.Metadata) {
        log.Printf("Now playing: %s - %s", m.Artist, m.Title)
    },
    OnError: func(err error) { log.Printf("client error: %v", err) },
})

player.Connect()
```

### ReceiverV2 — get raw decoded audio

`ReceiverV2` does connect → handshake → session → clock sync → stream
lifecycle → decode → schedule, and emits `audio.Buffer` on `Output()`. Use it
for visualizers, DSP, or a custom output backend.

## Examples

See [`examples/basic-player`](examples/basic-player/).

## Architecture

```
Network:  protocol/secure  (Noise transport, identities, psk, pairing tokens)
Session:  protocol/session (client establishment, activate rules, messaging)
Client:   sendspin        (ReceiverV2 + PlayerV2, scheduling, reconnect)
Sync:     sync            (Kalman time filter)
Audio:    audio           (codec decode, malgo output)
```

## Development

```bash
make test
make lint
```

Protocol conformance is documented in `docs/SPEC_COMPLIANCE.md`.

## License

See [LICENSE](LICENSE).
