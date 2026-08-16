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

	lag := crossCorrelateLag(videoPCM, audioPCM)
	return time.Duration(lag) * time.Second / syncSampleRate, nil
}

// crossCorrelateLag returns the sample lag k where audioPCM lags videoPCM by k
// samples: videoPCM[i] best matches audioPCM[i+k]. A positive lag means the
// dubbed audio starts later than the video's audio.
//
// Both passes use a mean-centered, normalized correlation (covariance divided
// by the number of overlapping samples). Without this normalization the raw
// dot-product naturally peaks near whichever lag has the most overlapping
// samples — which is lag 0 when both clips are the same length, but shifts
// unpredictably whenever the two decoded clips differ in length, producing a
// wrong offset exactly in that case.
func crossCorrelateLag(videoPCM, audioPCM []int16) int {
	// Bound the search to what the shorter decoded clip can actually support,
	// so we never search into lags with near-zero real overlap. Cap it so a
	// few seconds of shared signal always remain to correlate against.
	maxLag := len(videoPCM)
	if len(audioPCM) < maxLag {
		maxLag = len(audioPCM)
	}
	maxLag /= 2
	if limit := int(syncSampleSeconds-2) * syncSampleRate; maxLag > limit {
		maxLag = limit
	}
	if maxLag <= 0 {
		return 0
	}

	// Coarse pass: decimate both signals to ~500Hz (factor 16) and search every
	// 4th lag (0.5s resolution) — cheap and robust.
	const decimate = 16
	const lagStep = 4 * decimate
	v, a := decimatePCM(videoPCM, decimate), decimatePCM(audioPCM, decimate)
	vm, am := mean(v), mean(a)

	coarseBest := 0
	best := math.Inf(-1)
	for lag := -maxLag; lag <= maxLag; lag += lagStep {
		score, n := correlateScoreCentered(v, a, lag/decimate, vm, am)
		if n == 0 {
			continue
		}
		if normalized := score / float64(n); normalized > best {
			best = normalized
			coarseBest = lag
		}
	}

	// Fine pass: +/-0.5s around the coarse peak, at half the decimation, using
	// the same normalized metric so the refinement can't reintroduce the bias.
	const fineDecimate = 2
	vf, af := decimatePCM(videoPCM, fineDecimate), decimatePCM(audioPCM, fineDecimate)
	vfm, afm := mean(vf), mean(af)

	bestLag := coarseBest
	fineBest := math.Inf(-1)
	window := syncSampleRate / 2
	for lag := (coarseBest - window) / fineDecimate; lag <= (coarseBest+window)/fineDecimate; lag++ {
		score, n := correlateScoreCentered(vf, af, lag, vfm, afm)
		if n == 0 {
			continue
		}
		if normalized := score / float64(n); normalized > fineBest {
			fineBest = normalized
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

// correlateScoreCentered computes the mean-centered cross-correlation sum at
// the given lag together with the number of overlapping samples. Callers must
// divide by that count before comparing scores across different lags, since
// the raw sum grows with overlap size, not with correlation strength.
func correlateScoreCentered(videoPCM, audioPCM []int16, lag int, vm, am float64) (float64, int) {
	var sum float64
	var n int
	for i := 0; i < len(videoPCM); i++ {
		j := i + lag
		if j < 0 || j >= len(audioPCM) {
			continue
		}
		sum += (float64(videoPCM[i]) - vm) * (float64(audioPCM[j]) - am)
		n++
	}
	return sum, n
}
