# Changelog

Forked and reworked as **client-only** — the encrypted Sendspin client.

## [0.1.0] - 2026-11-10

### Changed

- Forked `Sendspin/sendspin-go` and stripped every server component: the v2
  server (`pkg/protocol/server`), the legacy engine and TUI (`internal/server`,
  `internal/ui`), mDNS discovery, the legacy player/server SDK code, and the
  server CLI.
- The product is the spec-conformant encrypted client: `ReceiverV2`/`PlayerV2`
  (`pkg/sendspin`), the Noise transport (`pkg/protocol/secure`), the client
  session and pairing (`pkg/protocol/session`), player-audio chunk parsing
  (`pkg/protocol/playback`), codec decode + malgo output (`pkg/audio`), and the
  Kalman clock filter (`pkg/sync`).
- Single CLI: `cmd/spinsync`; multi-architecture builds via
  `.github/workflows/v2-client.yml` and `Dockerfile`
  (amd64/arm64/armv7/armv6, including the Pi Zero W).
- Build on Go 1.27; module dependency set reduced (no TUI, discovery, mp3, or
  YAML dependencies).
