// ABOUTME: Entry point for the Sendspin client
// ABOUTME: Thin CLI wrapper around pkg/sendspin.PlayerV2 on the encrypted wire
//
// Runtime dependencies on a Raspberry Pi: libopus0 and ALSA (install
// libopus-dev + libasound2-dev to build, libopus0 at runtime). Cross builds
// for armv7/armv6 are produced by the ".github/workflows/v2-client.yml"
// pipeline; the armv6 build targets the Pi Zero / Zero W (use Debian's
// arm-linux-gnueabihf toolchain with -march=armv6 so the C audio stack is
// armv6 too, not just the Go code).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Sendspin/sendspin-go/internal/version"
	"github.com/Sendspin/sendspin-go/pkg/audio"
	"github.com/Sendspin/sendspin-go/pkg/protocol/secure"
	"github.com/Sendspin/sendspin-go/pkg/protocol/session"
	"github.com/Sendspin/sendspin-go/pkg/sendspin"
)

var (
	serverAddr     = flag.String("server", "", "Server address (host:port), e.g. 192.168.1.10:8927")
	name           = flag.String("name", "", "Player friendly name (default: hostname-sendspin-client)")
	identityPath   = flag.String("identity", defaultIdentityPath(), "Path to the client identity key (generated on first run, 0600)")
	bufferMs       = flag.Int("buffer-ms", 500, "Playback buffer size in milliseconds")
	outputDelayMs  = flag.Int("output-delay-ms", 0, "output_delay_ms reported to the server (0-5000)")
	bufferCapacity = flag.Int("buffer-capacity", 1048576, "buffer_capacity advertised to the server in bytes (default: 1MB)")
	maxSampleRate  = flag.Int("max-sample-rate", 0, "Cap the highest sample rate advertised to the server. 0 = no cap.")
	maxBitDepth    = flag.Int("max-bit-depth", 0, "Cap the highest bit depth advertised to the server. 0 = no cap.")
	unpairedAccess = flag.Bool("unpaired-access", true, "Admit unpaired servers (needed only until pairing is implemented in the client)")
	showVersion    = flag.Bool("version", false, "Print version and exit")
)

// defaultIdentityPath prefers $SENDSPIN_CLIENT_CONFIG's directory for the
// identity key, so it travels with the player config on the Pi.
func defaultIdentityPath() string {
	if dir := os.Getenv("SENDSPIN_CLIENT_CONFIG"); dir != "" {
		return filepath.Join(filepath.Dir(dir), "identity")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "sendspin", "identity")
	}
	return "sendspin-identity"
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	if *serverAddr == "" {
		log.Fatalf("-server is required (e.g. -server 192.168.1.10:8927)")
	}

	// Persistent identity: the public key is the client_id on the wire.
	id, err := secure.LoadOrGenerateIdentity(*identityPath)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	log.Printf("client_id: %s", id.ID())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p, err := sendspin.NewPlayerV2(sendspin.V2Config{
		ServerAddr:     *serverAddr,
		Identity:       id,
		Name:           *name,
		BufferMs:       *bufferMs,
		OutputDelayMs:  *outputDelayMs,
		BufferCapacity: *bufferCapacity,
		MaxSampleRate:  *maxSampleRate,
		MaxBitDepth:    *maxBitDepth,
		UnpairedAccess: *unpairedAccess,
		OnMetadata:     func(m sendspin.Metadata) { log.Printf("now playing: %s - %s", m.Artist, m.Title) },
		OnStreamStart: func(f audio.Format) {
			log.Printf("stream: %s %dHz %dch %dbit", f.Codec, f.SampleRate, f.Channels, f.BitDepth)
		},
		OnStreamEnd:   func() {},
		OnError:       func(err error) { log.Printf("client error: %v", err) },
		OnStateChange: func(s sendspin.PlayerState) { log.Printf("state: %s (connected=%v)", s.State, s.Connected) },
		OnCommand:     func(c session.PlayerCommand) {},
	})
	if err != nil {
		log.Fatalf("player: %v", err)
	}

	if err := p.Connect(); err != nil {
		log.Fatalf("connect to %s: %v", *serverAddr, err)
	}

	<-ctx.Done()
	log.Printf("shutting down")
	_ = p.Close()
}
