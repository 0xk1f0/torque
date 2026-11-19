# Changelog

Forked and reworked as **client-only** — the encrypted Sendspin client.

## [Unreleased]

### Fixed

- **Playback cutouts ("few seconds of audio, then ~10–15s silence, then
  resumption") from scheduler late-drop storms.** The scheduler dropped any
  buffer more than 50ms past its play time, but once the pipeline slipped that
  far behind — a shifted clock mapping, a decode stall, or a network hiccup —
  every subsequently arriving chunk was also late, so the client dropped the
  whole stream one-by-one and stayed silent until the ~10s clock-sync burst or a
  stream restart rescued it. `processQueue` now detects a desync storm (head of
  queue >250ms late), flushes the backlog, and re-enters buffering, bounding the
  outage to roughly `--buffer-ms` instead of a burst interval.
- Enriched the `Dropped late buffer` log with the chunk's server-side lead,
  clock-sync RTT/quality/age (`server_lead=…, rtt=…, quality=…, sync_age=…`) so
  a future occurrence shows whether the timestamp mapping or the pipeline is at
  fault, and added a periodic 5s `Scheduler stats` line for the timeline.
- **Decode moved off the network read loop.** `handleAudioChunk` used to decode
  inline on the single `readLoop` goroutine; on constrained devices CPU-bound
  codec work stalled network reads and starved `client/time` replies — a route
  into the late-drop storms above. Raw chunks now go through a bounded
  (128-entry ≈2.5s) FIFO to a dedicated decode worker, and each chunk carries a
  stream generation so async-decoded audio from a superseded track is dropped
  instead of bleeding into the new stream.

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
- Single CLI: `cmd/torque`; multi-architecture builds via
  `.github/workflows/v2-client.yml` and `Dockerfile`
  (amd64/arm64/armv7/armv6, including the Pi Zero W).
- Build on Go 1.27; module dependency set reduced (no TUI, discovery, mp3, or
  YAML dependencies).
