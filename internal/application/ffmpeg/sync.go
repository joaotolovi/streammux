package ffmpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// SyncSeconds is how much audio is decoded from each source to estimate the
// A/V offset. 15s of 8kHz mono PCM is ~240KB per source.
const SyncSeconds = 15.0

// syncSampleRate is the sample rate used for the offset estimate.
const syncSampleRate = 8000

// MaxSyncCandidateTracks limits how many video audio tracks we test for the
// offset estimate. The intended signal is the film's music and effects, shared
// with the dubbed track, so the main mix almost always sits in the first few
// candidates selected by channel count / default disposition.
const MaxSyncCandidateTracks = 4

// SyncMinConfidence is the minimum z-score for a correlation peak to be
// trusted. A genuinely shared track produces a sharp, isolated peak (high
// z-score); a mismatched pair (e.g. commentary x dub) produces noise whose
// "peak" barely rises above the mean. Calibrated empirically later.
const SyncMinConfidence = 4.0

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

	return rawToPCM(stdout.Bytes()), nil
}

// ExtractPCMMulti decodes several candidate audio tracks from a single source
// in one ffmpeg invocation, so the CDN input is fetched exactly once no matter
// how many tracks are being probed. This matters for sources with many
// untagged audio tracks: testing each candidate with a separate download is
// what triggers CDN timeouts/rate-limits; testing them in one pass does not.
//
// A missing track index (e.g. asking for 0:a:14 on a file with 10 tracks)
// makes ffmpeg exit non-zero even though the other outputs were written fine,
// so the error is only returned when no track at all was decoded.
func (m *Muxer) ExtractPCMMulti(ctx context.Context, url string, trackIndexes []int, seconds float64) (map[int][]int16, error) {
	if len(trackIndexes) == 0 {
		return nil, fmt.Errorf("extract pcm multi: no track indexes given")
	}

	tmpDir, err := os.MkdirTemp("", "streammux-sync-*")
	if err != nil {
		return nil, fmt.Errorf("extract pcm multi: temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	args := []string{
		"-v", "error",
		"-ss", "0",
		"-t", fmtDuration(seconds),
		"-i", url,
	}
	paths := make(map[int]string, len(trackIndexes))
	for _, idx := range trackIndexes {
		path := filepath.Join(tmpDir, fmt.Sprintf("track_%d.pcm", idx))
		paths[idx] = path
		args = append(args,
			"-map", fmt.Sprintf("0:a:%d", idx),
			"-ac", "1",
			"-ar", strconv.Itoa(syncSampleRate),
			"-f", "s16le",
			path,
		)
	}

	cmd := exec.CommandContext(ctx, m.binaryPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	out := make(map[int][]int16, len(trackIndexes))
	for idx, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil || len(raw) == 0 {
			continue
		}
		out[idx] = rawToPCM(raw)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("extract pcm multi: no tracks decoded: %w: %s", runErr, tail(stderr.String(), 500))
	}
	return out, nil
}

func rawToPCM(raw []byte) []int16 {
	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-len(raw)%2]
	}
	samples := make([]int16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		samples = append(samples, int16(binary.LittleEndian.Uint16(raw[i:i+2])))
	}
	return samples
}

// DetectAudioOffset estimates the time distance between the video source's
// audio and the dubbed audio track, returning how far the dubbed audio starts
// after the video source audio (positive = dub starts later), the video track
// that produced the winning peak, and its confidence z-score.
//
// The video's candidate tracks are decoded in a single ffmpeg call (one source
// fetch), and each is correlated against the dub. The track with the sharpest
// peak wins, because the music and effects shared with the dub live on the
// video's main mix which is not always the first track.
func (m *Muxer) DetectAudioOffset(videoURL, audioURL string, videoTracks []AudioTrack, audioTrackIndex int, seconds float64) (time.Duration, int, float64, error) {
	if len(videoTracks) == 0 {
		return 0, -1, 0, fmt.Errorf("no audio tracks on video source")
	}
	candidates := syncCandidates(videoTracks)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Video candidates and the dub are fetched concurrently: video in one call
	// (all tracks), dub in another.
	type videoResult struct {
		pcm map[int][]int16
		err error
	}
	type audioResult struct {
		pcm []int16
		err error
	}
	videoCh := make(chan videoResult, 1)
	audioCh := make(chan audioResult, 1)
	go func() {
		pcm, e := m.ExtractPCMMulti(ctx, videoURL, candidates, seconds)
		videoCh <- videoResult{pcm, e}
	}()
	go func() {
		pcm, e := m.ExtractPCM(ctx, audioURL, audioTrackIndex, seconds)
		audioCh <- audioResult{pcm, e}
	}()

	videoRes, audioRes := <-videoCh, <-audioCh
	if videoRes.err != nil {
		return 0, -1, 0, videoRes.err
	}
	if audioRes.err != nil {
		return 0, -1, 0, audioRes.err
	}
	if len(audioRes.pcm) < 8000 {
		return 0, -1, 0, fmt.Errorf("insufficient dub audio for sync (%d samples)", len(audioRes.pcm))
	}

	bestTrack, bestLag, bestConf := -1, 0, math.Inf(-1)
	for _, track := range candidates {
		pcm, ok := videoRes.pcm[track]
		if !ok || len(pcm) < 8000 {
			continue
		}
		lag, conf := crossCorrelateLag(pcm, audioRes.pcm)
		log.Printf("mux: sync candidate a:%d -> lag %.2fs confidence %.1f", track, float64(lag)/syncSampleRate, conf)
		if conf > bestConf {
			bestConf, bestLag, bestTrack = conf, lag, track
		}
	}
	if bestTrack == -1 {
		return 0, -1, 0, fmt.Errorf("no candidate video track had enough audio for sync")
	}
	return time.Duration(bestLag) * time.Second / syncSampleRate, bestTrack, bestConf, nil
}

// syncCandidates shortlists the video's audio streams most likely to carry the
// main mix: default disposition first, then most channels, then highest
// bitrate, then container order. Without language tags, channel count is the
// strongest signal — commentary and descriptive-audio tracks are almost always
// mono/stereo while the real mix is usually 5.1/7.1.
func syncCandidates(tracks []AudioTrack) []int {
	ordered := make([]AudioTrack, len(tracks))
	copy(ordered, tracks)
	sortTracks(ordered)
	if len(ordered) > MaxSyncCandidateTracks {
		ordered = ordered[:MaxSyncCandidateTracks]
	}
	idx := make([]int, len(ordered))
	for i, t := range ordered {
		idx[i] = t.Index
	}
	return idx
}

func sortTracks(tracks []AudioTrack) {
	for i := 1; i < len(tracks); i++ {
		for j := i; j > 0 && trackBetter(tracks[j], tracks[j-1]); j-- {
			tracks[j], tracks[j-1] = tracks[j-1], tracks[j]
		}
	}
}

func trackBetter(a, b AudioTrack) bool {
	if a.Default != b.Default {
		return a.Default
	}
	if a.Channels != b.Channels {
		return a.Channels > b.Channels
	}
	if a.BitRate != b.BitRate {
		return a.BitRate > b.BitRate
	}
	return a.Index < b.Index
}

// crossCorrelateLag returns the sample lag k where audioPCM lags videoPCM by k
// samples: videoPCM[i] best matches audioPCM[i+k]. A positive lag means the
// dubbed audio starts later than the video's audio. It also returns the
// confidence of the peak: the z-score of the best coarse-pass lag against the
// spread of all candidate lags.
//
// A genuine shared mix produces a sharp, isolated peak — a high z-score. A
// mismatched pair (e.g. a commentary or descriptive-audio track tested against
// the dub) produces scores that are all roughly noise, so the "peak" barely
// rises above the mean — a low z-score. That lets the caller reject a
// wrong-track match instead of confidently applying a meaningless offset.
//
// Both passes use a mean-centered, normalized correlation (covariance divided
// by the number of overlapping samples). Without this normalization the raw
// dot-product naturally peaks near whichever lag has the most overlapping
// samples — which is lag 0 when both clips are the same length, but shifts
// unpredictably whenever the two decoded clips differ in length.
func crossCorrelateLag(videoPCM, audioPCM []int16) (int, float64) {
	shorter := len(videoPCM)
	if len(audioPCM) < shorter {
		shorter = len(audioPCM)
	}
	const minOverlapFrac = 0.6
	const absoluteCapSeconds = 5.0
	maxLag := int(float64(shorter) * (1 - minOverlapFrac))
	if cap := int(absoluteCapSeconds * syncSampleRate); maxLag > cap {
		maxLag = cap
	}
	if maxLag <= 0 {
		return 0, 0
	}
	minOverlap := shorter - maxLag

	const coarseDecimate = 16
	const coarseStep = 4 * coarseDecimate
	v, a := decimatePCM(videoPCM, coarseDecimate), decimatePCM(audioPCM, coarseDecimate)
	vm, am := mean(v), mean(a)
	minOverlapDecimated := minOverlap / coarseDecimate

	var scores []float64
	coarseBest := 0
	best := math.Inf(-1)
	for l := -maxLag; l <= maxLag; l += coarseStep {
		score, n := correlateScoreCentered(v, a, l/coarseDecimate, vm, am)
		if n < minOverlapDecimated {
			continue
		}
		normalized := score / float64(n)
		scores = append(scores, normalized)
		if normalized > best {
			best = normalized
			coarseBest = l
		}
	}
	if len(scores) < 3 {
		return 0, 0
	}

	scoreMean, scoreStd := meanStd(scores)
	if scoreStd == 0 {
		return 0, 0
	}
	confidence := (best - scoreMean) / scoreStd

	const fineDecimate = 2
	window := syncSampleRate / 2
	loLag, hiLag := coarseBest-window, coarseBest+window
	if loLag < -maxLag {
		loLag = -maxLag
	}
	if hiLag > maxLag {
		hiLag = maxLag
	}
	lag := bestLagInRange(videoPCM, audioPCM, loLag, hiLag, fineDecimate, minOverlap)
	return lag, confidence
}

func bestLagInRange(videoPCM, audioPCM []int16, loLag, hiLag, decimate, minOverlap int) int {
	v, a := decimatePCM(videoPCM, decimate), decimatePCM(audioPCM, decimate)
	vm, am := mean(v), mean(a)
	bestLag, best := 0, math.Inf(-1)
	for lag := loLag / decimate; lag <= hiLag/decimate; lag++ {
		score, n := correlateScoreCentered(v, a, lag, vm, am)
		if n < minOverlap/decimate {
			continue
		}
		if normalized := score / float64(n); normalized > best {
			best = normalized
			bestLag = lag * decimate
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

func meanStd(xs []float64) (m, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	for _, x := range xs {
		d := x - m
		std += d * d
	}
	return m, math.Sqrt(std / float64(len(xs)))
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
