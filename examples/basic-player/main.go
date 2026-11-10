// ABOUTME: Minimal spec-v2 Sendspin client example
// ABOUTME: Connects a PlayerV2 to an encrypted server and plays audio
package main

import (
	"log"

	"github.com/Sendspin/sendspin-go/pkg/protocol/secure"
	"github.com/Sendspin/sendspin-go/pkg/sendspin"
)

func main() {
	// Identity: the client's persistent Curve25519 keypair. The public key is
	// the client_id on the wire; reuse the same file across reboots so the
	// server recognizes the player. GenerateIdentity is used here for brevity;
	// production clients persist the key (secure.LoadOrGenerateIdentity).
	id, err := secure.GenerateIdentity()
	if err != nil {
		log.Fatal(err)
	}

	player, err := sendspin.NewPlayerV2(sendspin.V2Config{
		ServerAddr: "localhost:8927",
		Identity:   id,
		Name:       "Example Client",
		BufferMs:   150,
		OnMetadata: func(m sendspin.Metadata) {
			log.Printf("now playing: %s - %s", m.Artist, m.Title)
		},
		OnError: func(err error) {
			log.Printf("client error: %v", err)
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	if err := player.Connect(); err != nil {
		log.Fatal(err)
	}
	log.Printf("client connected (%s), streaming...", id.ID())

	select {}
}
