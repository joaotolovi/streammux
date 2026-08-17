package muxer

import (
	"log"
	"time"

	"github.com/streammux/streammux/internal/domain/model"
)

// deliverySample is one completed segment HTTP response to the player.
type deliverySample struct {
	at      time.Time
	bytes   int64
	seconds float64
}

const (
	// deliveryPauseGap resets the throughput window after a pause: no
	// deliveries for this long means the player is idle, and the elapsed
	// wall time must not count against the measurement.
	deliveryPauseGap = 15 * time.Second
	// deliveryWindow is how many recent deliveries are considered.
	deliveryWindow = 4
)

// ObserveDelivery records one completed video segment delivery to the player
// and downgrades the active plan when the sustained player throughput cannot
// keep up with the plan's bitrate. Audio segments are deliberately not
// observed: they are tiny and would pollute the measurement.
func (m *Muxer) ObserveDelivery(job *model.MuxJob, sent int64, elapsed time.Duration) {
	if sent <= 0 || elapsed <= 0 {
		return
	}
	state := m.lookupState(job.ID)
	if state == nil {
		return
	}

	state.mu.Lock()
	state.lastAccess = time.Now()
	now := time.Now()
	samples := state.deliveries
	if len(samples) > 0 && now.Sub(samples[len(samples)-1].at) > deliveryPauseGap {
		samples = nil
	}
	samples = append(samples, deliverySample{at: now, bytes: sent, seconds: elapsed.Seconds()})
	if len(samples) > deliveryWindow {
		samples = samples[len(samples)-deliveryWindow:]
	}
	state.deliveries = samples

	active := state.active
	required := 0.0
	if active != nil && active.prepared != nil {
		required = active.prepared.videoBitrate
		if required <= 0 {
			required = float64(active.plan.EstimatedBandwidth())
		}
		// Headroom: the delivery must also carry audio, TS overhead and
		// leave margin for the player's own buffer consumption.
		required *= 1.25
	}
	tooSlow := playerTooSlow(samples, required)

	segment := state.lastRequested
	nextPlan := state.nextPlan
	recovering := state.recovering
	cooldown := time.Since(state.lastRecovery) >= m.policy.RecoveryCooldown
	state.mu.Unlock()

	if !tooSlow || recovering || !cooldown || nextPlan >= len(job.Plans) || segment < 0 {
		return
	}
	log.Printf("mux: player throughput below %.1f Mbps sustained; downgrading to plan %d at segment %d",
		required/1e6, nextPlan, segment)
	m.ensureRecovery(job, state, segment, nextPlan, "player throughput")
}

// playerTooSlow reports whether every recent delivery was slower than the
// required bitrate. Requiring all samples in the window avoids false
// positives from a single jittery delivery.
func playerTooSlow(samples []deliverySample, required float64) bool {
	if required <= 0 || len(samples) < deliveryWindow {
		return false
	}
	for _, s := range samples {
		if s.bytes <= 0 || s.seconds <= 0 {
			return false
		}
		if float64(s.bytes*8)/s.seconds >= required {
			return false
		}
	}
	return true
}
