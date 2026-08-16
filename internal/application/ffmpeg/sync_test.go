package ffmpeg

import (
	"math"
	"testing"
)

// syntheticBurst builds a signal with a normalized sine wave plus a loud burst.
func syntheticBurst(samples int, burstStart, burstLen int) []int16 {
	pcm := make([]int16, samples)
	for i := range pcm {
		pcm[i] = int16(math.Sin(float64(i)/40.0) * 6000)
	}
	for i := burstStart; i < burstStart+burstLen && i < samples; i++ {
		pcm[i] = 30000
	}
	return pcm
}

func TestCrossCorrelateLagDetectsShift(t *testing.T) {
	// Build a synthetic signal with a distinctive burst, then a delayed copy.
	video := syntheticBurst(15*syncSampleRate, 1*syncSampleRate, 2000)

	shift := 2 * syncSampleRate // audio delayed by 2s
	audio := make([]int16, len(video)+shift)
	for i, s := range video {
		audio[i+shift] = s
	}

	lag := crossCorrelateLag(video, audio)
	want := shift
	// Allow +/-200ms tolerance due to coarse+fine search resolution.
	if math.Abs(float64(lag-want)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~%d, got %d", want, lag)
	}
}

func TestCrossCorrelateLagNoShift(t *testing.T) {
	video := syntheticBurst(10*syncSampleRate, 1*syncSampleRate, 2000)
	audio := make([]int16, len(video))
	copy(audio, video)

	lag := crossCorrelateLag(video, audio)
	if math.Abs(float64(lag)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~0, got %d", lag)
	}
}

// TestCrossCorrelateLagDCBias verifies that a shared DC offset on both signals
// does not pull the estimate toward zero (mean centering in both passes).
func TestCrossCorrelateLagDCBias(t *testing.T) {
	video := syntheticBurst(15*syncSampleRate, 1*syncSampleRate, 2000)
	shift := 1500 // audio delayed by ~187ms
	audio := make([]int16, len(video)+shift)
	for i, s := range video {
		audio[i+shift] = s
	}
	// Add the same DC offset to both.
	for i := range video {
		video[i] += 2000
	}
	for i := range audio {
		audio[i] += 2000
	}

	lag := crossCorrelateLag(video, audio)
	if math.Abs(float64(lag-shift)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~%d, got %d", shift, lag)
	}
}

// TestCrossCorrelateLagUnequalLengths verifies that differing clip lengths do
// not bias the estimate via raw (unnormalized) overlap counts.
func TestCrossCorrelateLagUnequalLengths(t *testing.T) {
	video := syntheticBurst(15*syncSampleRate, 1*syncSampleRate, 2000)
	shift := 2500 // audio delayed by ~312ms
	audio := append(make([]int16, shift), video...)
	// Truncate the audio clip so one source is notably shorter than the other.
	audio = audio[:12*syncSampleRate]

	lag := crossCorrelateLag(video, audio)
	if math.Abs(float64(lag-shift)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~%d, got %d", shift, lag)
	}
}
