package ffmpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"time"
)

// syncSampleSeconds is how much audio is decoded from each source to estimate
// the A/V offset. 15s of 8kHz mono PCM is ~240KB per source.
const syncSampleSeconds = 15.0

// syncSampleRate is the sample rate used for the offset estimate.
const syncSampleRate = 8000

// ExtractPCM decodes up to seconds of the given audio track to 8kHz mono s16le
// PCM. It seeks to zero, so it captures the start of the actual audio content.
func (m *Muxer) ExtractPCM(ctx context.Context, url string, trackIndex int, seconds float64) ([]int16, error) {
	args := []string{
		"-v", "error",
		"-ss", "0",
		"-t", fmtDuration(seconds),
		"-i", url,
		"-map", fmt.Sprintf("0:a:%d", trackIndex),
		"-ac", "1",
		"-ar", strconv.Itoa(syncSampleRate),
		"-f", "s16le",
		"pipe:1",
	}

	cmd := exec.CommandContext(ctx, m.binaryPath, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("extract pcm: %w: %s", err, tail(stderr.String(), 500))
	}

	raw := stdout.Bytes()
	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-len(raw)%2]
	}
	samples := make([]int16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		samples = append(samples, int16(binary.LittleEndian.Uint16(raw[i:i+2])))
	}
	return samples, nil
}

// EstimateAudioOffset cross-correlates two mono PCM signals and returns the
// offset in seconds by which signal b lags behind signal a (positive means b
// starts later). It decodes up to syncSampleSeconds from each source.
//
// The two signals are the source video's audio track and the dubbed audio
// track. Music and sound effects are shared between releases even when the
// dialogue language differs, so the cross-correlation peak reveals how much to
// shift the dubbed audio to align with the video.
func (m *Muxer) EstimateAudioOffset(ctx context.Context, videoURL, audioURL string, videoTrackIndex, audioTrackIndex int) (time.Duration, error) {
	sampleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// The two downloads are independent, so fetch them concurrently to halve
	// the wall-clock cost of the estimate.
	type result struct {
		pcm []int16
		err error
	}
	videoCh := make(chan result, 1)
	audioCh := make(chan result, 1)
	go func() {
		pcm, err := m.ExtractPCM(sampleCtx, videoURL, videoTrackIndex, syncSampleSeconds)
		videoCh <- result{pcm, err}
	}()
	go func() {
		pcm, err := m.ExtractPCM(sampleCtx, audioURL, audioTrackIndex, syncSampleSeconds)
		audioCh <- result{pcm, err}
	}()

	videoRes := <-videoCh
	audioRes := <-audioCh
	if videoRes.err != nil {
		return 0, fmt.Errorf("extract video audio: %w", videoRes.err)
	}
	if audioRes.err != nil {
		return 0, fmt.Errorf("extract dubbed audio: %w", audioRes.err)
	}

	videoPCM, audioPCM := videoRes.pcm, audioRes.pcm
	if len(videoPCM) < 8000 || len(audioPCM) < 8000 {
		return 0, fmt.Errorf("insufficient audio for sync (%d and %d samples)", len(videoPCM), len(audioPCM))
	}

	lag := crossCorrelateLag(videoPCM, audioPCM, syncSampleRate)
	return time.Duration(lag) * time.Second / syncSampleRate, nil
}

// crossCorrelateLag returns the sample lag k where audioPCM[i] best matches
// videoPCM[i+k], i.e. how many samples audioPCM is behind videoPCM. A positive
// lag means the dubbed audio starts later than the video's audio; shifting the
// audio input forward by k samples (negative itsoffset) would align them.
//
// The search is done at a reduced rate for speed, then refined around the peak
// at the full rate. Signals are centered (mean removed) so that loud scenes
// common to both tracks dominate the correlation.
func crossCorrelateLag(videoPCM, audioPCM []int16, _ int) int {
	// Bound the search to +/-3s, which covers intro differences between releases.
	const maxLag = 3 * syncSampleRate

	// Coarse pass: decimate both signals to ~500Hz (factor 16) and search every
	// 4th lag (0.5s resolution) — cheap and robust.
	const decimate = 16
	const lagStep = 4 * decimate
	v, a := decimatePCM(videoPCM, decimate), decimatePCM(audioPCM, decimate)

	coarseBest := 0
	best := math.Inf(-1)
	for lag := -maxLag; lag <= maxLag; lag += lagStep {
		score := correlateScore(v, a, lag/decimate)
		if score > best {
			best = score
			coarseBest = lag
		}
	}

	// Fine pass: +/-0.5s around the coarse peak at half rate (4kHz), using only
	// the first few seconds of signal — the intro alignment is what matters.
	const fineDecimate = 2
	const fineWindowSeconds = 4
	vf := decimatePCM(videoPCM[:min(len(videoPCM), fineWindowSeconds*syncSampleRate)], fineDecimate)
	af := decimatePCM(audioPCM, fineDecimate)
	vm, am := mean(vf), mean(af)

	bestLag := coarseBest
	fineBest := math.Inf(-1)
	for lag := (coarseBest - syncSampleRate/2) / fineDecimate; lag <= (coarseBest+syncSampleRate/2)/fineDecimate; lag++ {
		score := correlateScoreCentered(vf, af, lag, vm, am)
		if score > fineBest {
			fineBest = score
			bestLag = lag * fineDecimate
		}
	}
	return bestLag
}

// decimatePCM downsamples by averaging blocks, preserving signal energy.
func decimatePCM(pcm []int16, factor int) []int16 {
	out := make([]int16, 0, len(pcm)/factor)
	for i := 0; i+factor <= len(pcm); i += factor {
		var sum int64
		for j := 0; j < factor; j++ {
			sum += int64(pcm[i+j])
		}
		out = append(out, int16(sum/int64(factor)))
	}
	return out
}

func mean(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum int64
	for _, s := range pcm {
		sum += int64(s)
	}
	return float64(sum) / float64(len(pcm))
}

// correlateScore computes the cross-correlation at the given lag without
// centering; used for the coarse pass where only the relative peak matters.
func correlateScore(videoPCM, audioPCM []int16, lag int) float64 {
	var sum int64
	for i := 0; i < len(videoPCM); i++ {
		j := i + lag
		if j < 0 || j >= len(audioPCM) {
			continue
		}
		sum += int64(videoPCM[i]) * int64(audioPCM[j])
	}
	return float64(sum)
}

// correlateScoreCentered computes the mean-centered cross-correlation at the
// given lag, which removes any DC offset from both signals.
func correlateScoreCentered(videoPCM, audioPCM []int16, lag int, vm, am float64) float64 {
	var sum float64
	for i := 0; i < len(videoPCM); i++ {
		j := i + lag
		if j < 0 || j >= len(audioPCM) {
			continue
		}
		sum += (float64(videoPCM[i]) - vm) * (float64(audioPCM[j]) - am)
	}
	return sum
}
