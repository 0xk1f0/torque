# torque

A Go **client** for the [Sendspin Protocol](https://github.com/Sendspin/spec): a
Noise-encrypted player that connects to a Sendspin server and plays
synchronized audio. Client-only fork of
[`Sendspin/sendspin-go`](https://github.com/Sendspin/sendspin-go).

## Build

Requires Go ≥ 1.27, `pkg-config`, `libopus`, and ALSA on Linux.

```bash
go build -o torque ./cmd/torque
go test ./...
```

Cross-build all Raspberry Pi architectures + x86_64. The `golang` base image has
no 32-bit armv6 variant, so `armv6` is built from the `arm/v7` image with
`ARM_VARIANT=v6` to force armv6 codegen (both Go and the cgo audio stack):

<details>
<summary>docker (buildx)</summary>

```bash
for p in linux/amd64 linux/arm64 linux/arm/v7 linux/arm/v6; do
  plat="$p"; extra=""
  if [ "$p" = linux/arm/v6 ]; then
    plat=linux/arm/v7; extra="--build-arg ARM_VARIANT=v6"
  fi
  docker buildx build --platform "$plat" $extra --target export \
    --output type=local,dest="dist/${p//\//_}" .
done
```

Needs binfmt/QEMU registration first: `docker run --privileged tonistiigi/binfmt:latest --install all` and `docker buildx create --use`.
</details>

<details>
<summary>podman</summary>

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
</details>

Each build drops a `torque` binary into `dist/<platform>/`. The same matrix runs
in CI: `.github/workflows/client.yml` (which cross-compiles directly with Go
toolchains and is unaffected by the Dockerfile).

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
sudo install -m 644 scripts/systemd/torque.service /etc/systemd/system/
sudo install -m 644 scripts/systemd/torque.env /etc/default/torque
sudo systemctl daemon-reload && sudo systemctl enable --now torque
```

Configure the server and options in `/etc/default/torque`.

## License

[LICENSE](LICENSE) (Community Specification License 1.0).
