package ffmpeg

import (
	"math"
	"testing"
)

// syntheticBurst builds a deterministic broadband signal (pseudo-random noise
// plus a rising chirp) with a loud burst. Broadband content matters: a pure
// tone has periodic autocorrelation that inflates every correlation lag, so it
// cannot discriminate a true alignment from a spurious one the way real film
// audio does.
func syntheticBurst(samples int, burstStart, burstLen int) []int16 {
	pcm := make([]int16, samples)
	state := uint32(12345)
	for i := range pcm {
		state = 1664525*state + 1013904223
		pcm[i] = int16(int64(state>>8)%30000 - 15000)
	}
	for i := burstStart; i < burstStart+burstLen && i < samples; i++ {
		f := 200.0 + 1800.0*float64(i-burstStart)/float64(burstLen)
		pcm[i] = int16(28000 * math.Sin(2*math.Pi*f*float64(i)/float64(syncSampleRate)))
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

	lag, _ := crossCorrelateLag(video, audio)
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

	lag, _ := crossCorrelateLag(video, audio)
	if math.Abs(float64(lag)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~0, got %d", lag)
	}
}

// TestCrossCorrelateLagDCBias verifies that a shared DC offset on both signals
// does not pull the estimate toward zero (mean centering in both passes).
func TestCrossCorrelateLagDCBias(t *testing.T) {
	video := syntheticBurst(15*syncSampleRate, 1*syncSampleRate, 2000)
	shift := 2 * syncSampleRate // audio delayed by 2s
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

	lag, _ := crossCorrelateLag(video, audio)
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

	lag, _ := crossCorrelateLag(video, audio)
	if math.Abs(float64(lag-shift)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~%d, got %d", shift, lag)
	}
}

// TestCrossCorrelateLagConfidence verifies that a clear shared-signal match
// yields a high z-score and that unrelated audio does not.
func TestCrossCorrelateLagConfidence(t *testing.T) {
	// Broadband shared signal with a distinctive chirp burst: a delayed copy
	// must correlate strongly at the true lag with a high z-score.
	video := syntheticBurst(15*syncSampleRate, 1*syncSampleRate, 2000)
	shift := 3 * syncSampleRate
	audio := make([]int16, len(video)+shift)
	for i, s := range video {
		audio[i+shift] = s
	}
	lag, conf := crossCorrelateLag(video, audio)
	if math.Abs(float64(lag-shift)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~%d, got %d", shift, lag)
	}
	if conf < SyncMinConfidence {
		t.Fatalf("expected high confidence for shared signal, got %v", conf)
	}

	// Uncorrelated broadband noise → no dominant peak → low z-score.
	noiseA := make([]int16, 15*syncSampleRate)
	noiseB := make([]int16, len(noiseA))
	for i := range noiseA {
		noiseA[i] = int16((i*7919 + 13) % 40000) - 20000
		noiseB[i] = int16((i*104729 + 7) % 40000) - 20000
	}
	_, confNoise := crossCorrelateLag(noiseA, noiseB)
	if confNoise >= SyncMinConfidence {
		t.Fatalf("expected low confidence for uncorrelated noise, got %v", confNoise)
	}
}

func TestCandidateTracksOrdersPrimaryFirst(t *testing.T) {
	tracks := []AudioTrack{
		{Index: 0, Channels: 2, BitRate: 128000, Default: false},
		{Index: 1, Channels: 6, BitRate: 640000, Default: true},
		{Index: 2, Channels: 6, BitRate: 384000, Default: false},
		{Index: 3, Channels: 2, BitRate: 768000, Default: false},
	}
	// syncCandidates returns indexes trimmed to MaxSyncCandidateTracks (4 here).
	// Priority: default, then most channels, then bitrate, then index.
	ordered := syncCandidates(tracks)
	if len(ordered) != 4 {
		t.Fatalf("expected 4 candidates, got %d", len(ordered))
	}
	if ordered[0] != 1 {
		t.Fatalf("expected default 5.1 first, got index %d", ordered[0])
	}
	if ordered[1] != 2 {
		t.Fatalf("expected higher-channel 5.1 second, got index %d", ordered[1])
	}
	if ordered[2] != 3 || ordered[3] != 0 {
		t.Fatalf("unexpected relative order: %d,%d", ordered[2], ordered[3])
	}
}
