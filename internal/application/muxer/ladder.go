package muxer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

// The ABR downgrade ladder. Tier 0 is the selected source and lower tiers are
// on-the-fly transcodes of that same source. Arbitrary releases cannot safely
// share HLS boundaries, timestamps, or audio with the primary.

const (
	tierCount        = 3
	transcodeQuality = 0.65 // realtime x264 veryfast vs a professional encode
)

// strategyKind identifies what a ladder rung does.
type strategyKind int

const (
	stratSource strategyKind = iota
	stratTranscode
)

// tierStrategy is one rung of a tier's downgrade ladder.
type tierStrategy struct {
	kind      strategyKind
	video     *sourceState          // stratSource: the lighter source to switch to
	audio     *sourceState          // stratSource: pairing (usually the current dub)
	transcode *ffmpeg.TranscodeSpec // stratTranscode: params for re-encoding the primary source
	score     int                   // expected delivered quality, ranks the rungs
	estBits   int64                 // committed/estimated bitrate for ABR metadata
	height    int                   // target height for ABR metadata
	desc      string                // human-readable, for logs
}

// tierTargets returns the per-tier bitrate targets derived from the primary
// plan's bitrate. Each tier aims 20% lighter than the primary: tier 1 ~80%,
// tier 2 ~60%. The values are targets, not ceilings — the ladder ranks all
// lighter sources by distance to the target so degradation is gradual: no
// source is discarded, the closest to the target is tried first, then the
// next closest on failure, etc. Caps keep the tiers well spaced (100/50/10
// is more stable than 100/90/80) when the primary is huge.
func tierTargets(t0Bits int64) [tierCount]int64 {
	if t0Bits <= 0 {
		t0Bits = 8_000_000
	}
	var b [tierCount]int64
	b[0] = t0Bits
	// 20% and 40% lighter than primary, capped so tiers stay distant for
	// huge primaries (80 GB REMUX should not have tier 1 at 62 Mbps).
	b[1] = min64(25_000_000, t0Bits*80/100)
	if b[1] >= t0Bits {
		b[1] = t0Bits * 80 / 100
	}
	b[2] = min64(10_000_000, t0Bits*60/100)
	if b[2] >= b[1] {
		b[2] = b[1] * 75 / 100
	}
	return b
}

// transcodeSpecFor picks sensible transcode parameters for a tier given the
// primary source height.
func transcodeSpecFor(tier, t0Height int) *ffmpeg.TranscodeSpec {
	switch tier {
	case 1:
		if t0Height > 1080 {
			return &ffmpeg.TranscodeSpec{Height: 1080, MaxRateKbps: 18000, Preset: "veryfast"}
		}
		return &ffmpeg.TranscodeSpec{Height: 720, MaxRateKbps: 10000, Preset: "veryfast"}
	default: // tier 2
		if t0Height > 720 {
			return &ffmpeg.TranscodeSpec{Height: 720, MaxRateKbps: 6000, Preset: "veryfast"}
		}
		return &ffmpeg.TranscodeSpec{Height: 480, MaxRateKbps: 4000, Preset: "veryfast"}
	}
}

func transcodePrepared(source *preparedPlan, spec *ffmpeg.TranscodeSpec) *preparedPlan {
	clone := *source
	clone.transcode = spec
	clone.videoBitrate = float64(spec.MaxRateKbps) * 1000
	clone.videoCodec = "h264"
	if source.videoWidth > 0 && source.videoHeight > 0 {
		clone.videoWidth = source.videoWidth * spec.Height / source.videoHeight
		clone.videoHeight = spec.Height
	}
	return &clone
}

// buildTierLadder returns only a transcode of the selected primary source.
// Stream-copy variants from other releases have unrelated keyframe boundaries
// and cannot be switched safely on this fixed public segment timeline.
func buildTierLadder(state *playbackState, tier int) []*tierStrategy {
	if tier <= 0 || tier >= tierCount || state.tier0Prepared == nil {
		return nil
	}
	t0 := state.tier0Prepared
	target := state.tierBudgets[tier]
	if target <= 0 {
		target = state.tierBudgets[0] * 8 / 10
		if target <= 0 {
			target = 8_000_000
		}
	}
	tc := transcodeSpecFor(tier, t0.videoHeight)
	tcBits := int64(tc.MaxRateKbps) * 1000
	tcScore := int(float64(analyzer.VideoScore(t0.plan.Video)) * transcodeQuality)
	tcDist := tcBits - target
	if tcDist < 0 {
		tcDist = -tcDist
	}
	strategy := &tierStrategy{
		kind:      stratTranscode,
		transcode: tc,
		score:     tcScore,
		estBits:   tcBits,
		height:    tc.Height,
		desc:      fmt.Sprintf("transcode primary -> %dp@%dk", tc.Height, tc.MaxRateKbps),
	}
	log.Printf("mux: tier %d transcode target=%dk bitrate=%dk height=%dp score=%d distance=%dk", tier, target/1000, strategy.estBits/1000, strategy.height, strategy.score, tcDist/1000)
	return []*tierStrategy{strategy}
}

// strategyUnderBudget prefers a real source that already satisfies the tier's
// bitrate budget over a realtime transcode. This avoids spending CPU when the
// addon already supplied a usable lighter rendition.
func strategyUnderBudget(strategy *tierStrategy, target int64) bool {
	return strategy.kind == stratSource && strategy.estBits <= target
}

// resolutionHeight maps a parsed resolution string to pixels for ABR metadata.
func resolutionHeight(res string) int {
	switch res {
	case "2160p":
		return 2160
	case "1440p":
		return 1440
	case "1080p":
		return 1080
	case "720p":
		return 720
	case "576p":
		return 576
	case "480p":
		return 480
	}
	return 0
}

// streamBandwidth estimates a video source's bitrate in bits/s from its
// metadata: advertised peak bitrate when present, else the resolution/encode
// heuristic from PlaybackPlan.EstimatedBandwidth.
func streamBandwidth(s model.CollectedStream) int64 {
	if s.VideoBitrate > 0 {
		return s.VideoBitrate
	}
	return int64(model.PlaybackPlan{Video: s}.EstimatedBandwidth())
}

// prepareTierStrategy resolves and probes a rung into a launchable plan.
// Source rungs share the composer ledger: a failed probe marks the source
// dead for every pairing.
func (m *Muxer) prepareTierStrategy(ctx context.Context, job *model.MuxJob, state *playbackState, s *tierStrategy) (*preparedPlan, error) {
	switch s.kind {
	case stratTranscode:
		t0 := state.tier0Prepared
		if t0 == nil {
			return nil, fmt.Errorf("no primary plan to transcode")
		}
		return transcodePrepared(t0, s.transcode), nil
	case stratSource:
		comp := &composition{
			video:   s.video,
			audio:   s.audio,
			single:  s.video == s.audio,
			ordinal: 1000 + s.video.videoPos, // synthetic ordinal, unique per source
		}
		prepared, _, err := m.prepareComposition(ctx, job, comp)
		return prepared, err
	}
	return nil, fmt.Errorf("unknown strategy kind")
}

// requestTier records an ABR request and starts at most one background switch.
// The active generation continues serving the requested namespace until the
// target is ready. A request for the active tier cancels a pending switch;
// this avoids preparing a rendition the player has already abandoned.
func (m *Muxer) requestTier(job *model.MuxJob, state *playbackState, tier, atSegment int) {
	if tier < 0 || tier >= tierCount {
		return
	}

	var cancel context.CancelFunc
	state.mu.Lock()
	if state.closed || state.recovering || state.tier0Prepared == nil {
		state.mu.Unlock()
		return
	}
	if state.tierBusy {
		if state.tierPending != tier {
			cancel = state.tierSwitchCancel
		}
		state.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	if state.activeTier == tier {
		state.mu.Unlock()
		return
	}
	if !state.lastTierSwitch.IsZero() && time.Since(state.lastTierSwitch) < m.policy.TierSwitchCooldown {
		state.mu.Unlock()
		return
	}

	switchCtx, switchCancel := context.WithCancel(state.ctx)
	state.tierBusy = true
	state.tierPending = tier
	state.tierSwitchCancel = switchCancel
	state.tierWait = make(chan struct{})
	state.tierErr = nil
	state.mu.Unlock()

	go m.runTierSwitch(job, state, tier, atSegment, switchCtx, switchCancel)
}

// runTierSwitch walks the tier's ladder at the player's position, falling
// through to deeper tiers' ladders when a whole tier is exhausted. The
// resulting generation serves the requesting tier's namespace regardless of
// which ladder ultimately supplied it.
func (m *Muxer) runTierSwitch(job *model.MuxJob, state *playbackState, tier, atSegment int, switchCtx context.Context, switchCancel context.CancelFunc) {
	start := time.Now()
	winner, fromTier, err := m.launchTierStrategy(job, state, tier, atSegment, switchCtx)
	switchCancel()

	state.mu.Lock()
	wait := state.tierWait
	old := state.active
	committed := false
	if err == nil && !state.closed && state.tierPending == tier {
		committed = true
		state.active = winner
		state.all = append(state.all, winner)
		state.activeTier = tier
		state.lastTierSwitch = time.Now()
		state.deliveries = nil // samples measured the previous tier's bitrate
		if old != nil && old != winner && old.prepared != nil && winner.prepared != nil {
			// Mark the cutover in this tier's video playlist; the shared
			// (audio) timeline only breaks when the audio pairing changes.
			state.tierDisc[tier] = appendUniqueInt(state.tierDisc[tier], atSegment)
			if old.prepared.plan.Audio.SourceKey() != winner.prepared.plan.Audio.SourceKey() {
				state.discontinuities = appendUniqueInt(state.discontinuities, atSegment)
			}
		}
	} else {
		state.tierErr = err
	}
	state.tierBusy = false
	state.tierPending = -1
	state.tierSwitchCancel = nil
	state.tierWait = nil
	state.mu.Unlock()

	if wait != nil {
		close(wait)
	}
	if err != nil || !committed {
		if !committed && winner != nil {
			winner.session.Cancel()
			m.removeGenerationWhenStopped(winner)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("mux: tier %d switch failed after %s: %v", tier, time.Since(start).Round(time.Millisecond), err)
		}
		return
	}
	if old != nil && old != winner {
		// The target has a buffered segment at the cutover. Stop the old
		// generation immediately so a tier switch never leaves three sessions
		// alive and the normal state returns to one generation.
		old.session.Cancel()
		m.removeGenerationWhenStopped(old)
	}
	log.Printf("mux: tier %d -> %s (ladder tier %d) at segment %d in %s",
		tier, strategyDesc(winner.prepared), fromTier, atSegment, time.Since(start).Round(time.Millisecond))
	go m.monitorGeneration(job, state, winner)
}

// launchTierStrategy tries every rung of the tier's ladder, then deeper
// tiers' ladders, returning the first generation that produces a segment.
func (m *Muxer) launchTierStrategy(job *model.MuxJob, state *playbackState, tier, atSegment int, switchCtx context.Context) (*generation, int, error) {
	var lastErr error
	for t := tier; t < tierCount; t++ {
		ladder := buildTierLadder(state, t)
		for _, s := range ladder {
			ctx, cancel := context.WithTimeout(switchCtx, m.policy.StartupTimeout)
			prepared, err := m.prepareTierStrategy(ctx, job, state, s)
			if err != nil {
				cancel()
				if errors.Is(err, context.Canceled) || errors.Is(switchCtx.Err(), context.Canceled) {
					return nil, tier, context.Canceled
				}
				lastErr = err
				log.Printf("mux: tier %d strategy %s failed to prepare: %v", tier, s.desc, err)
				continue
			}
			state.mu.Lock()
			filmBase := state.filmBase
			filmStartTime := state.filmStartTime
			state.mu.Unlock()
			startTime := filmSourceTime(filmBase, filmStartTime, atSegment)
			gen, err := m.launchGenerationContext(job, state, switchCtx, 1000+t, prepared, tier, atSegment, startTime, m.policy.TierSwitchBuffer)
			cancel()
			if err != nil {
				lastErr = err
				if errors.Is(err, context.Canceled) || errors.Is(switchCtx.Err(), context.Canceled) {
					return nil, tier, context.Canceled
				}
				log.Printf("mux: tier %d strategy %s failed to launch: %v", tier, s.desc, err)
				continue
			}
			return gen, t, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no strategies available")
	}
	return nil, tier, lastErr
}

func strategyDesc(p *preparedPlan) string {
	if p == nil {
		return "unknown"
	}
	if p.transcode != nil {
		return fmt.Sprintf("transcode %dp@%dk", p.transcode.Height, p.transcode.MaxRateKbps)
	}
	return fmt.Sprintf("video#%d + audio#%d", p.videoIdx, p.audioIdx)
}

// tierMetasFromPlans computes the ABR variant metadata for the master
// playlist from plan metadata only (no probes). It mirrors buildTierLadder's
// target model: tier 0 is the primary, tiers 1-2 aim 20%/40% lighter (80%/60%
// of the primary).
//
// The BANDWIDTH announced for each tier is the SMALLER of:
//   - the estimated bitrate of the best strategy for that tier, or
//   - the tier's minimum target (tier0 × 0.80 / × 0.60).
//
// When the real strategy is lighter than the minimum (e.g. 30% below tier 0
// when the minimum is 20%), the player sees the real value — its ABR decision
// is precise. When the real strategy is barely lighter than tier 0 (e.g. only
// 10% below), the player sees the 20% minimum instead — forcing enough
// spacing so the player doesn't switch for no reason between near-identical
// bitrates.
func tierMetasFromPlans(plans []model.PlaybackPlan) []model.TierMeta {
	var primary *model.PlaybackPlan
	for i := range plans {
		if plans[i].HasTargetAudio {
			primary = &plans[i]
			break
		}
	}
	if primary == nil {
		return nil
	}

	t0Bits := streamBandwidth(primary.Video)
	t0Height := resolutionHeight(primary.Video.Parsed.Resolution)
	targets := tierTargets(t0Bits)

	metas := make([]model.TierMeta, tierCount)
	metas[0] = model.TierMeta{Bandwidth: t0Bits * 12 / 10, Height: t0Height}
	if h := metas[0].Height; h > 0 {
		metas[0].Width = h * 16 / 9
	}

	seen := map[string]bool{primary.Video.SourceKey(): true}
	for tier := 1; tier < tierCount; tier++ {
		target := targets[tier]
		// Best existing source closest to the target (among all lighter than t0).
		var best *model.CollectedStream
		bestDist := int64(1<<62 - 1)
		bestScore := -1
		for i := range plans {
			v := &plans[i].Video
			key := v.SourceKey()
			if seen[key] || !plans[i].HasTargetAudio {
				continue
			}
			est := streamBandwidth(*v)
			if est <= 0 || est >= t0Bits {
				continue
			}
			dist := est - target
			if dist < 0 {
				dist = -dist
			}
			score := analyzer.VideoScore(*v)
			if dist < bestDist || (dist == bestDist && score > bestScore) {
				bestDist = dist
				bestScore = score
				best = v
			}
		}
		// Transcode candidate for this tier as fallback.
		tc := transcodeSpecFor(tier, t0Height)
		tcBits := int64(tc.MaxRateKbps) * 1000
		tcDist := tcBits - target
		if tcDist < 0 {
			tcDist = -tcDist
		}
		// Pick whichever is closer to the target; tie-break by quality.
		tcScore := int(float64(analyzer.VideoScore(primary.Video)) * transcodeQuality)
		useTranscode := best == nil || tcDist < bestDist || (tcDist == bestDist && tcScore > bestScore)

		var estBits int64
		var height int
		if useTranscode {
			estBits = tcBits
			height = tc.Height
		} else {
			seen[best.SourceKey()] = true
			estBits = streamBandwidth(*best)
			height = resolutionHeight(best.Parsed.Resolution)
		}

		// The BANDWIDTH is the smaller of the real estimate and the tier's
		// minimum target. When the real strategy is lighter than the minimum,
		// the player sees reality; when it's barely lighter, the player sees
		// the forced minimum so it doesn't switch between near-identical tiers.
		announced := min64(estBits, target)
		if announced <= 0 {
			announced = target
		}
		metas[tier] = model.TierMeta{Bandwidth: announced * 12 / 10, Height: height}
		if height > 0 {
			metas[tier].Width = height * 16 / 9
		}
	}
	return metas
}

// placeholderTierMetas gives the player a stable virtual ladder on the very
// first master request. The values are replaced with addon-derived estimates
// as soon as preparation completes, and tier 0 is replaced again by probed
// dimensions/bitrate when the selected source starts.
func placeholderTierMetas() []model.TierMeta {
	return []model.TierMeta{
		{Bandwidth: 12_000_000, Width: 1920, Height: 1080},
		{Bandwidth: 6_000_000, Width: 1280, Height: 720},
		{Bandwidth: 2_500_000, Width: 854, Height: 480},
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func appendUniqueInt(list []int, v int) []int {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
