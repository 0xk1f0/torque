# Basic Player Example

A minimal spec-v2 Sendspin client: connects to a Sendspin server over the
encrypted (Noise) wire and plays synchronized audio.

## What it demonstrates

- Persistent client identity (`secure.LoadOrGenerateIdentity`; the public key
  is the `client_id` on the wire)
- Encrypted session establishment (`PlayerV2.Connect`)
- Clock synchronization and stream lifecycle handled by the client
- Metadata and error callbacks

## Running

```bash
cd examples/basic-player
go build
./basic-player -server 192.168.1.100:8927
```

The example uses a throwaway identity for brevity. A real client persists the
key so the server recognizes the player across reboots:

```go
id, _ := secure.LoadOrGenerateIdentity("/home/pi/.config/sendspin/identity")
```

## Next steps

- Full CLI with reconnect, output-delay, buffer tuning: `cmd/sendspin-client/`
- Library docs: `pkg/sendspin/`
