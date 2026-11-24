// ABOUTME: High-level Sendspin client API
// ABOUTME: Provides the encrypted Player/Receiver client for most use cases

// Package sendspin provides high-level APIs for the Sendspin client: an
// encrypted (Noise) player that connects to a Sendspin server and plays
// synchronized audio.
//
// The main entry point is Player, which composes a Receiver (connect,
// handshake, session, clock sync, stream lifecycle, decode, scheduling) with
// an audio output backend.
//
// For lower-level control, see the protocol (transport/session/pairing),
// audio (codecs, output), and sync (clock filter) packages.
//
// Example:
//
//	player, err := sendspin.NewPlayer(sendspin.Config{
//	    ServerAddr: "localhost:8927",
//	    Identity:   id, // *secure.Identity, persisted across reboots
//	    Name:       "Living Room",
//	})
//	err = player.Connect()
package sendspin
