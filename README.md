# torque

A Go **client** for the [Sendspin Protocol](https://github.com/Sendspin/spec): a
Noise-encrypted player that connects to a Sendspin server and plays
synchronized audio. Client-only fork of
[`Sendspin/sendspin-go`](https://github.com/Sendspin/sendspin-go).

## Build

Requires Go ≥ 1.27, `pkg-config`, `libopus`, and ALSA on Linux.

```bash
make            # builds ./torque
make test       # go test ./...
```

## Run

```bash
torque --server 192.168.1.10:8927 --name "Living Room" --device snd_rpi_hifiberry_dacplus
```

- `--server` is required (`host:port`).
- `--name` sets the player name (default: `hostname-torque`).
- `--device`/`-d` selects the playback device (see `--list-devices`/`-l`;
  empty = platform default).
- `--identity`/`-i` sets the identity key path (default `~/.config/torque/identity`,
  generated on first run; the client store lives alongside it at `store.json`).
- `--help` lists all flags.

Pairing is automatic: when the server requests it, torque shows a 6-digit
(dynamic) code to enter in Home Assistant and saves the long-term PSK, so
later runs connect already paired. Unpaired access is not supported.

## Service

```bash
make install-daemon   # install + enable systemd unit, configure /etc/default/torque
```

## License

[LICENSE](LICENSE) (Community Specification License 1.0).
