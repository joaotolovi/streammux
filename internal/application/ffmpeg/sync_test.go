package ffmpeg

import (
	"math"
	"testing"
)

func TestCrossCorrelateLagDetectsShift(t *testing.T) {
	// Build a synthetic signal with a distinctive burst, then a delayed copy.
	video := make([]int16, 15*syncSampleRate)
	for i := range video {
		video[i] = int16(math.Sin(float64(i)/40.0) * 6000)
	}
	// Burst at 1s in video.
	for i := 1 * syncSampleRate; i < (1*syncSampleRate + 2000); i++ {
		video[i] = 30000
	}

	shift := 2 * syncSampleRate // audio delayed by 2s
	audio := make([]int16, len(video)+shift)
	for i, s := range video {
		audio[i+shift] = s
	}

	lag := crossCorrelateLag(video, audio, syncSampleRate)
	want := shift
	// Allow +/-200ms tolerance due to coarse+fine search resolution.
	if math.Abs(float64(lag-want)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~%d, got %d", want, lag)
	}
}

func TestCrossCorrelateLagNoShift(t *testing.T) {
	video := make([]int16, 10*syncSampleRate)
	audio := make([]int16, len(video))
	for i := range video {
		v := int16(math.Sin(float64(i)/30.0) * 8000)
		video[i] = v
		audio[i] = v
	}

	lag := crossCorrelateLag(video, audio, syncSampleRate)
	if math.Abs(float64(lag)) > 0.2*syncSampleRate {
		t.Fatalf("expected lag ~0, got %d", lag)
	}
}
