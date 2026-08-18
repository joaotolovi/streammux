package muxer

import (
	"log"
	"time"

	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

// deliverySample is one completed segment HTTP response to the player.
type deliverySample struct {
	// at is the approximate request start (completion time minus the
	// transfer duration). Using the start keeps gaps between samples
	// meaningful: a slow transfer no longer looks like player idleness.
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
	deliveryWindow = 3
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
	samples := appendDeliverySample(state.deliveries, now.Add(-elapsed), sent, elapsed)
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
	recovering := state.recovering
	cooldown := time.Since(state.lastRecovery) >= m.policy.RecoveryCooldown
	state.mu.Unlock()

	if !tooSlow {
		// Diagnostics: log the latest delivery throughput whenever it is
		// below the required bitrate so player-side bottlenecks (network
		// or decode) are visible in the logs even before a downgrade
		// accumulates enough evidence.
		last := samples[len(samples)-1]
		if required > 0 && last.bytes > 0 && float64(last.bytes*8)/last.seconds < required {
			log.Printf("mux: delivery %.1f Mbps below required %.1f Mbps (%.1f MB in %.1fs; window %d/%d)",
				float64(last.bytes*8)/last.seconds/1e6, required/1e6,
				float64(last.bytes)/1e6, last.seconds, len(samples), deliveryWindow)
		}
		return
	}
	if recovering || !cooldown || segment < 0 {
		log.Printf("mux: player throughput below %.1f Mbps sustained but downgrade deferred (recovering=%v cooldown=%v segment=%d)",
			required/1e6, recovering, !cooldown, segment)
		return
	}
	log.Printf("mux: player throughput below %.1f Mbps sustained; downgrading to lighter sources at segment %d",
		required/1e6, segment)
	m.ensureRecovery(job, state, segment, "player throughput")
}

// appendDeliverySample appends one delivery to the window, resetting the
// window only after a genuine player pause. The idle time is the gap between
// request starts minus how long the previous transfer itself took; without
// that subtraction, a slow player whose deliveries take longer than the gap
// would reset the window on every sample and the downgrade could never
// accumulate evidence.
func appendDeliverySample(samples []deliverySample, started time.Time, sent int64, elapsed time.Duration) []deliverySample {
	if len(samples) > 0 {
		last := samples[len(samples)-1]
		idle := started.Sub(last.at) - time.Duration(last.seconds*float64(time.Second))
		if idle > deliveryPauseGap {
			samples = nil
		}
	}
	samples = append(samples, deliverySample{at: started, bytes: sent, seconds: elapsed.Seconds()})
	if len(samples) > deliveryWindow {
		samples = samples[len(samples)-deliveryWindow:]
	}
	return samples
}

// playerTooSlow reports whether every recent delivery was slower than the
// required bitrate. Requiring all samples in the window avoids false
// positives from a single jittery delivery — except for a catastrophic
// delivery (one transfer taking more than 3x the segment duration), which is
// evidence enough on its own: the buffer is visibly draining.
func playerTooSlow(samples []deliverySample, required float64) bool {
	if required <= 0 || len(samples) == 0 {
		return false
	}
	catastrophic := 3 * time.Duration(ffmpeg.SegDuration()*float64(time.Second))
	for _, s := range samples {
		if s.bytes <= 0 || s.seconds <= 0 {
			return false
		}
		if time.Duration(s.seconds*float64(time.Second)) >= catastrophic {
			return true
		}
	}
	if len(samples) < deliveryWindow {
		return false
	}
	for _, s := range samples {
		if float64(s.bytes*8)/s.seconds >= required {
			return false
		}
	}
	return true
}
