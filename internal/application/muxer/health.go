package muxer

import (
	"log"
	"time"

	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

type healthDecision struct {
	realtime  float64
	downgrade bool
}

type healthTracker struct {
	window     time.Duration
	minimum    float64
	samples    []ffmpeg.ProgressSample
	lastEval   time.Time
	lowWindows int
}

func newHealthTracker(policy Policy) *healthTracker {
	return &healthTracker{window: policy.HealthWindow, minimum: policy.MinRealtime}
}

// observe derives a moving production rate from FFmpeg's real session:
// media seconds produced / monotonic wall-clock seconds. Two non-overlapping
// slow windows are required before a downgrade is recommended.
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
	if wall < h.window*3/4 || media <= 0 {
		return healthDecision{}
	}
	realtime := float64(media) / float64(wall)
	decision := healthDecision{realtime: realtime}

	if !h.lastEval.IsZero() && sample.At.Sub(h.lastEval) < h.window {
		return decision
	}
	h.lastEval = sample.At
	if realtime < h.minimum {
		h.lowWindows++
	} else {
		h.lowWindows = 0
	}
	decision.downgrade = h.lowWindows >= 2
	return decision
}

func (m *Muxer) monitorGeneration(job *model.MuxJob, state *playbackState, generation *generation) {
	tracker := newHealthTracker(m.policy)
	for sample := range generation.session.Progress() {
		if !m.isActiveGeneration(state, generation) {
			return
		}
		decision := tracker.observe(sample)
		if decision.realtime > 0 {
			log.Printf(
				"mux: plan %d session production %.2fx (ffmpeg cumulative %.2fx)",
				generation.planIndex,
				decision.realtime,
				sample.Speed,
			)
		}
		if !decision.downgrade {
			continue
		}

		state.mu.Lock()
		highest := highestCompleteSegment(generation.dir)
		requested := state.lastRequested
		ahead := time.Duration(highest-requested) * time.Duration(ffmpeg.SegDuration()*float64(time.Second))
		cooldownElapsed := time.Since(state.lastRecovery) >= m.policy.RecoveryCooldown
		nextPlan := state.nextPlan
		alreadyRecovering := state.recovering
		state.mu.Unlock()

		if requested < 0 || ahead >= m.policy.MinPublishedAhead || !cooldownElapsed || alreadyRecovering || nextPlan >= len(job.Plans) {
			continue
		}
		startSegment := highest + 1
		if startSegment <= requested {
			startSegment = requested + 1
		}
		log.Printf(
			"mux: plan %d is unsustainable at %.2fx with %s buffered; trying plan %d",
			generation.planIndex,
			decision.realtime,
			ahead.Round(time.Second),
			nextPlan,
		)
		m.ensureRecovery(job, state, startSegment, nextPlan, "measured FFmpeg throughput")
	}

	<-generation.session.Done()
	if generation.session.Err() == nil || !m.isActiveGeneration(state, generation) {
		return
	}

	state.mu.Lock()
	requested := state.lastRequested
	nextPlan := state.nextPlan
	recovering := state.recovering
	state.mu.Unlock()
	if requested < 0 {
		requested = highestCompleteSegment(generation.dir) + 1
	}
	if !recovering {
		m.ensureRecovery(job, state, requested, nextPlan, "active FFmpeg session failed")
	}
}

func (m *Muxer) isActiveGeneration(state *playbackState, generation *generation) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return !state.closed && state.active == generation
}
