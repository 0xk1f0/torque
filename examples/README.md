# Sendspin Examples

This directory contains example programs for the Sendspin client.

## Available Examples

### [basic-player](basic-player/)

A minimal spec-v2 client that connects to a Sendspin server over the
encrypted (Noise) wire and plays synchronized audio.

**Key features:**
- Encrypted connection (persistent identity key)
- Simple configuration with callbacks
- Automatic clock synchronization
- Metadata display

**Run it:**
```bash
cd basic-player
go build
./basic-player -server localhost:8927
```

## Quick Start

You'll need a Sendspin server (e.g. from the `Sendspin/sendspin-go` main
repository) on port 8927, then:

```bash
cd examples/basic-player
go build && ./basic-player -server localhost:8927
```

## Key Concepts

### Client API (`pkg/sendspin`)

```go
id, _ := secure.LoadOrGenerateIdentity("identity.key") // persisted keypair
player, _ := sendspin.NewPlayerV2(sendspin.V2Config{
    ServerAddr: "localhost:8927",
    Identity:   id,
    Name:       "Living Room",
    OnMetadata: func(m sendspin.Metadata) {
        log.Printf("Now playing: %s - %s", m.Artist, m.Title)
    },
})
player.Connect()
```

## Next Steps

- Full CLI: `cmd/torque/`
- Library docs: `pkg/sendspin/`
- Lower-level APIs: `pkg/audio/`, `pkg/protocol/`, `pkg/sync/`

## Troubleshooting

**No audio playing?**
- Verify the server is reachable: `nc -vz <server> 8927`
- Check your output device: `torque --help`
- Watch logs for clock-sync or connection errors

**Connection refused?**
- Confirm the server is running and the address is correct
