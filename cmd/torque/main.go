// ABOUTME: Entry point for the Sendspin client
// ABOUTME: Thin CLI wrapper around pkg/sendspin.Player on the encrypted wire
//
// Runtime dependencies on a Raspberry Pi: libopus0 and ALSA (install
// libopus-dev + libasound2-dev to build, libopus0 at runtime). Cross builds
// for armv7/armv6 are produced by the ".github/workflows/client.yml"
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

	"github.com/0xk1f0/torque/internal/version"
	"github.com/0xk1f0/torque/pkg/audio"
	"github.com/0xk1f0/torque/pkg/audio/output"
	"github.com/0xk1f0/torque/pkg/protocol/secure"
	"github.com/0xk1f0/torque/pkg/protocol/session"
	"github.com/0xk1f0/torque/pkg/sendspin"
)

var (
	serverAddr       string
	name             string
	identityPath     string
	bufferMs         int
	outputDelayMs    int
	bufferCapacity   int
	maxSampleRate    int
	maxBitDepth      int
	unpairedAccess   bool
	showVersion      bool
	showPairingToken bool
	deviceName       string
	listDevices      bool
)

func init() {
	// Long options use double-dash (--server …) as shown in help; Go's flag
	// package accepts both -- and - prefixes. Short aliases share the same
	// backing variable so either spelling sets the same value.
	flag.StringVar(&serverAddr, "server", "", "Server address (host:port), e.g. 192.168.1.10:8927")
	flag.StringVar(&serverAddr, "s", "", "shorthand for --server")

	flag.StringVar(&name, "name", "", "Player friendly name (default: hostname-torque)")
	flag.StringVar(&name, "n", "", "shorthand for --name")

	flag.StringVar(&identityPath, "identity", defaultIdentityPath(), "Path to the client identity key (generated on first run, 0600)")
	flag.StringVar(&identityPath, "i", defaultIdentityPath(), "shorthand for --identity")

	flag.IntVar(&bufferMs, "buffer-ms", 500, "Playback buffer size in milliseconds")
	flag.IntVar(&bufferMs, "b", 500, "shorthand for --buffer-ms")

	flag.IntVar(&outputDelayMs, "output-delay-ms", 0, "output_delay_ms reported to the server (0-5000)")
	flag.IntVar(&bufferCapacity, "buffer-capacity", 1048576, "buffer_capacity advertised to the server in bytes (default: 1MB)")
	flag.IntVar(&maxSampleRate, "max-sample-rate", 0, "Cap the highest sample rate advertised to the server. 0 = no cap.")
	flag.IntVar(&maxBitDepth, "max-bit-depth", 0, "Cap the highest bit depth advertised to the server. 0 = no cap.")

	flag.BoolVar(&unpairedAccess, "unpaired-access", false, "Admit unpaired servers: let a server this client has not paired with play audio without going through dynamic pin-code pairing. Off by default (pairing required). Enable only for servers without pairing support.")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.BoolVar(&showVersion, "v", false, "shorthand for --version")

	flag.BoolVar(&showPairingToken, "pairing-token", false, "Print this client's Pairing PSK token (SP:0...; enter it on a server to begin Pairing PSK pairing) and exit")

	flag.StringVar(&deviceName, "device", "", "Playback device name as reported by --list-devices (e.g. \"bcm2835 Headphones\" or its short part before \", \"). Empty = miniaudio platform default. Fails loudly if the device is not found, instead of silently falling back to a null/discard backend.")
	flag.StringVar(&deviceName, "d", "", "shorthand for --device")

	flag.BoolVar(&listDevices, "list-devices", false, "Enumerate the playback devices miniaudio/ALSA can see and exit")
	flag.BoolVar(&listDevices, "l", false, "shorthand for --list-devices")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage: %s [options]

Options (long forms accept a double dash; aliases are single dash):

  -s,  --server <addr>          Server address (host:port), e.g. 192.168.1.10:8927
  -n,  --name <name>            Player friendly name (default: hostname-torque)
  -i,  --identity <path>        Path to the client identity key (generated on first run, 0600)
  -d,  --device <name>          Playback device (see --list-devices; empty = platform default,
                                fails loudly instead of silently falling back to a discard backend)
  -b,  --buffer-ms <ms>         Playback buffer size in milliseconds (default 500)
       --output-delay-ms <ms>   output_delay_ms reported to the server (0-5000)
       --buffer-capacity <b>    buffer_capacity advertised to the server in bytes (default 1MB)
       --max-sample-rate <hz>   Cap the highest sample rate advertised to the server. 0 = no cap.
       --max-bit-depth <bits>   Cap the highest bit depth advertised to the server. 0 = no cap.
       --unpaired-access        Admit unpaired servers (play without dynamic pin-code pairing).
                                Off by default; enable only for servers without pairing support.
  -l,  --list-devices           Enumerate the playback devices miniaudio/ALSA can see and exit
       --pairing-token          Print this client's Pairing PSK token (SP:0...) and exit
  -v,  --version                Print version and exit
  -h,  --help                   Show this help

Write host:port as e.g. 192.168.1.10:8927.
`, os.Args[0])
	}
}

// defaultIdentityPath prefers $TORQUE_CONFIG's directory for the identity
// key, so it travels with the player config on the Pi.
func defaultIdentityPath() string {
	if dir := os.Getenv("TORQUE_CONFIG"); dir != "" {
		return filepath.Join(filepath.Dir(dir), "identity")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "torque", "identity")
	}
	return "torque-identity"
}

func main() {
	flag.Parse()

	if showVersion {
		fmt.Println(version.Version)
		return
	}

	// Discover the devices miniaudio/ALSA actually sees, then exit. This is the
	// knob that turns "no audio because torque picked the null/discard backend"
	// into a solvable setup step: the operator pastes a name from this list into
	// --device.
	if listDevices {
		devices, err := output.ListPlaybackDevices()
		if err != nil {
			log.Fatalf("list devices: %v", err)
		}
		if len(devices) == 0 {
			fmt.Println("no playback devices found — no real ALSA sound card is visible to this process")
			return
		}
		for _, d := range devices {
			mark := ""
			if d.IsDefault {
				mark = "  (default)"
			}
			fmt.Printf("%s%s\n", d.Name, mark)
		}
		return
	}

	if serverAddr == "" {
		log.Fatalf("--server is required (e.g. --server 192.168.1.10:8927)")
	}

	// A requested non-empty device is resolved (and fails loudly if absent) at
	// stream open, so a typo or an invisible sound card can never silently drop
	// audio into the null/discard backend.
	var out output.Output
	if deviceName != "" {
		out = output.NewMalgo(deviceName)
	}

	// Persistent identity: the public key is the client_id on the wire.
	id, err := secure.LoadOrGenerateIdentity(identityPath)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	log.Printf("client_id: %s", id.ID())

	// Name defaults to <hostname>-torque when not set.
	playerName := name
	if playerName == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			playerName = hn + "-torque"
		} else {
			playerName = "torque"
		}
	}

	// One unified client store lives in the identity's directory and carries
	// everything the client remembers across restarts: pairing records and the
	// player volume/mute. It is not separately configurable; it always travels
	// with the identity.
	store, err := sendspin.NewFileStore(filepath.Join(filepath.Dir(filepath.Clean(identityPath)), "store.json"))
	if err != nil {
		log.Fatalf("client store: %v", err)
	}

	// Print the Pairing PSK token and exit: the operator enters the SP:0
	// token into a server to begin Pairing PSK pairing. Generating it
	// provisions a persistent pairing PSK on first use.
	if showPairingToken {
		tok, err := sendspin.PairingToken(id, store)
		if err != nil {
			log.Fatalf("pairing token: %v", err)
		}
		fmt.Println(tok)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p, err := sendspin.NewPlayer(sendspin.Config{
		ServerAddr:     serverAddr,
		Identity:       id,
		Name:           playerName,
		BufferMs:       bufferMs,
		OutputDelayMs:  outputDelayMs,
		BufferCapacity: bufferCapacity,
		MaxSampleRate:  maxSampleRate,
		MaxBitDepth:    maxBitDepth,
		UnpairedAccess: unpairedAccess,
		Output:         out,
		DeviceInfo: sendspin.DeviceInfo{
			ProductName:  "torque", // model
			Manufacturer: "torque",
		},
		Store: store,
		PairingCodeEmitted: func(code string) {
			log.Printf("Enter this pairing code in Home Assistant: %s", code)
		},
		Reconnect:  sendspin.ReconnectConfig{Enabled: true},
		OnMetadata: func(m sendspin.Metadata) { log.Printf("now playing: %s - %s", m.Artist, m.Title) },
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
		log.Fatalf("connect to %s: %v", serverAddr, err)
	}

	<-ctx.Done()
	log.Printf("shutting down")
	_ = p.Close()
}
