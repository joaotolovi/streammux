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

// ObserveDelivery records one completed video segment delivery to the player.
// ABR decisions belong to the player, which selects a lower HLS variant from
// the master playlist. Audio segments are deliberately not observed: they are
// tiny and would pollute the measurement.
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
		// Headroom for audio + TS overhead only (player ABR disabled).
		required *= 1.05
	}
	state.mu.Unlock()

	last := samples[len(samples)-1]
	if required > 0 && last.bytes > 0 && float64(last.bytes*8)/last.seconds < required {
		sustained := windowThroughput(samples)
		log.Printf("mux: delivery %.1f Mbps below required %.1f Mbps (%.1f MB in %.1fs; window %d/%d sustained %.1f Mbps)",
			float64(last.bytes*8)/last.seconds/1e6, required/1e6,
			float64(last.bytes)/1e6, last.seconds, len(samples), deliveryWindow, sustained/1e6)
	}
	if playerTooSlow(samples, required) {
		sustained := windowThroughput(samples)
		state.mu.Lock()
		recovering := state.recovering
		cooldownElapsed := time.Since(state.lastRecovery) >= m.policy.RecoveryCooldown
		requested := state.lastRequested
		highest := -1
		if state.active != nil {
			highest = highestCompleteSegment(state.active.dir)
		}
		state.mu.Unlock()
		if !recovering && cooldownElapsed && requested >= 0 && highest >= 0 {
			next := requested + 1
			if next <= highest {
				next = highest + 1
			}
			log.Printf("mux: player throughput %.1f Mbps below required %.1f Mbps, switching to lighter source at segment %d", sustained/1e6, required/1e6, next)
			m.ensureRecovery(job, state, next, recoveryPlayerThroughput)
		}
	}
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

// windowThroughput returns the aggregate sustained throughput of the window:
// total bits delivered divided by total transfer time. Idle gaps between
// transfers are excluded — they mean the player was buffered, not slow.
func windowThroughput(samples []deliverySample) float64 {
	var bits, seconds float64
	for _, s := range samples {
		if s.bytes <= 0 || s.seconds <= 0 {
			continue
		}
		bits += float64(s.bytes) * 8
		seconds += s.seconds
	}
	if seconds <= 0 {
		return 0
	}
	return bits / seconds
}

// playerTooSlow reports whether the player's sustained delivery throughput
// cannot keep up with the required bitrate. It compares the AGGREGATE
// throughput of the window (total bits / total transfer time) against the
// requirement: a marginal link oscillating around the threshold drains the
// player's buffer over time, and a per-sample "all below" rule would be
// vetoed by every fast burst. A single catastrophic delivery (one transfer
// taking >= 3x the segment duration) is evidence enough on its own.
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
	return windowThroughput(samples) < required
}
