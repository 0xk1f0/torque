// ABOUTME: Volume and mute helpers shared by audio output backends
// ABOUTME: Extracted from oto.go during the oto removal cleanup
package output

import (
	"math"

	"github.com/0xk1f0/torque/pkg/audio"
)

// applyVolume applies volume and mute to samples with clipping protection.
// Samples are expected to be in the int32 24-bit range.
func applyVolume(samples []int32, volume int, muted bool) []int32 {
	return applyVolumeScratch(samples, volume, muted, nil)
}

// applyVolumeScratch is applyVolume with a reusable scratch buffer so steady
// playback does not allocate a fresh result slice per chunk. When scratch is
// non-nil it is grown as needed and reused on the next call; the caller must
// own it exclusively (e.g. guarded by the output mutex).
func applyVolumeScratch(samples []int32, volume int, muted bool, scratch *[]int32) []int32 {
	multiplier := getVolumeMultiplier(volume, muted)

	var result []int32
	if scratch != nil {
		if cap(*scratch) < len(samples) {
			*scratch = make([]int32, len(samples))
		}
		result = (*scratch)[:len(samples)]
	} else {
		result = make([]int32, len(samples))
	}
	for i, sample := range samples {
		scaled := int64(float64(sample) * multiplier)

		if scaled > audio.Max24Bit {
			scaled = audio.Max24Bit
		} else if scaled < audio.Min24Bit {
			scaled = audio.Min24Bit
		}

		result[i] = int32(scaled)
	}

	return result
}

// getVolumeMultiplier returns the float multiplier for a given volume/mute state.
// Volume is mapped through the Sendspin perceptual curve (vol/100)^1.5 so the
// gain tracks perceived loudness rather than raw amplitude. A linear vol/100
// mapping outputs ~0.5 (-6 dB) at volume 50 where a conformant player outputs
// ~0.354 (-9 dB); in a multi-room group that divergence makes this client
// audibly louder than its peers at any volume below 100.
func getVolumeMultiplier(volume int, muted bool) float64 {
	if muted {
		return 0.0
	}
	return math.Pow(float64(volume)/100.0, 1.5)
}
