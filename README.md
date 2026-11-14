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

Cross-build all Raspberry Pi architectures + x86_64:

<details>
<summary>docker (buildx)</summary>

```bash
docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6 \
    --target export --output type=local,dest=dist .
```

Needs binfmt/QEMU registration first: `docker run --privileged tonistiigi/binfmt:latest --install all` and `docker buildx create --use`.
</details>

<details>
<summary>podman (no buildx)</summary>

```bash
for p in linux/amd64 linux/arm64 linux/arm/v7 linux/arm/v6; do
  plat="$p"; extra=""
  if [ "$p" = linux/arm/v6 ]; then
    plat=linux/arm/v7; extra="--build-arg ARM_VARIANT=v6"
  fi
  podman build --platform "$plat" $extra --target export \
    --output type=local,dest="dist/${p//\//_}" .
done
```

Podman doesn't support the `v6` platform variant (buildx sets it via `TARGETVARIANT`), so the armv6 cell builds on `linux/arm/v7` and forces armv6 codegen with `ARM_VARIANT=v6`, so both Go and the cgo audio stack match the Pi Zero CPU.
</details>

Each build drops a `torque` binary into `dist/<platform>/`. Same matrix runs in CI: `.github/workflows/v2-client.yml` (which cross-compiles directly and is unaffected by the Dockerfile).

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
