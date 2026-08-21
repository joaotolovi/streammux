package muxer

import (
	"log"
	"time"

	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

type healthDecision struct {
	realtime float64
	valid    bool
}

type healthTracker struct {
	window  time.Duration
	samples []ffmpeg.ProgressSample
}

func newHealthTracker(policy Policy) *healthTracker {
	return &healthTracker{window: policy.HealthWindow}
}

func (h *healthTracker) reset() {
	h.samples = nil
}

// observe derives a smoothed production rate from FFmpeg's real session:
// media seconds produced / monotonic wall-clock seconds. The caller combines
// it with the on-disk media reserve to decide whether a handoff is needed.
func (h *healthTracker) observe(sample ffmpeg.ProgressSample) healthDecision {
	if sample.OutTime <= 0 || sample.At.IsZero() {
		return healthDecision{}
	}
	h.samples = append(h.samples, sample)
	cutoff := sample.At.Add(-h.window)
	first := 0
	for first < len(h.samples)-1 && h.samples[first].At.Before(cutoff) {
		first++
	}
	if first > 0 {
		h.samples = append([]ffmpeg.ProgressSample(nil), h.samples[first:]...)
	}
	if len(h.samples) < 2 {
		return healthDecision{}
	}

	oldest := h.samples[0]
	wall := sample.At.Sub(oldest.At)
	media := sample.OutTime - oldest.OutTime
	if wall < h.window*3/4 || media < 0 {
		return healthDecision{}
	}
	realtime := float64(media) / float64(wall)
	return healthDecision{realtime: realtime, valid: true}
}

// needsSourceHandoff reports whether an active viewer has reached the media
// reserve required to replace a source. The old source is stopped before the
// replacement starts, so ahead (not ahead divided by the current deficit) is
// the actual time available for the handoff.
func needsSourceHandoff(ahead time.Duration, production float64, reserve time.Duration) bool {
	return production >= 0 && production < 1 && ahead <= reserve
}

func (m *Muxer) monitorGeneration(job *model.MuxJob, state *playbackState, generation *generation) {
	tracker := newHealthTracker(m.policy)
	var playbackEpoch uint64
	for sample := range generation.session.Progress() {
		if !m.isActiveGeneration(state, generation) {
			return
		}

		state.mu.Lock()
		if playbackEpoch != state.playbackEpoch {
			playbackEpoch = state.playbackEpoch
			tracker.reset()
		}
		state.mu.Unlock()
		decision := tracker.observe(sample)
		if decision.valid {
			log.Printf(
				"mux: plan %d session production %.2fx (ffmpeg cumulative %.2fx)",
				generation.planIndex,
				decision.realtime,
				sample.Speed,
			)
		}
		if !decision.valid {
			continue
		}

		state.mu.Lock()
		highest := highestCompleteSegment(generation.dir)
		requested := state.lastRequested
		ahead := time.Duration(highest-requested) * time.Duration(ffmpeg.SegDuration()*float64(time.Second))
		playing := !state.lastSequentialAt.IsZero() && sample.At.Sub(state.lastSequentialAt) <= m.policy.HealthWindow*3
		cooldownElapsed := time.Since(state.lastRecovery) >= m.policy.RecoveryCooldown
		alreadyRecovering := state.recovering
		state.mu.Unlock()

		if requested < 0 || !playing || !cooldownElapsed || alreadyRecovering {
			continue
		}
		if !needsSourceHandoff(ahead, decision.realtime, m.policy.MinHandoffBuffer) {
			continue
		}

		startSegment := highest + 1
		if startSegment <= requested {
			startSegment = requested + 1
		}
		log.Printf(
			"mux: production %.2fx reached handoff reserve (%s buffered); switching sources",
			decision.realtime,
			ahead.Round(time.Second),
		)
		m.ensureRecovery(job, state, startSegment, recoveryProductionDeficit)
	}

	<-generation.session.Done()
	if generation.session.Err() == nil || !m.isActiveGeneration(state, generation) {
		return
	}

	state.mu.Lock()
	requested := state.lastRequested
	recovering := state.recovering
	state.mu.Unlock()
	if requested < 0 {
		requested = highestCompleteSegment(generation.dir) + 1
	}
	if !recovering {
		m.ensureRecovery(job, state, requested, "active FFmpeg session failed")
	}
}

func (m *Muxer) isActiveGeneration(state *playbackState, generation *generation) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return !state.closed && state.active == generation
}
