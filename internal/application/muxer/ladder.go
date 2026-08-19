package muxer

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

// The ABR downgrade ladder. The master playlist advertises tier 0/1/2 as
// progressively lighter variants. Tiers are severity signals, never bindings:
// when the player requests a tier, the server walks that tier's ladder of
// strategies — a lighter existing source or an on-the-fly transcode of the
// primary source — and serves whichever launches, falling through to the next
// strategy on failure and, if a whole tier is exhausted, to the next tier's
// ladder under the requested playlist. The public segment timeline is shared,
// so every switch resumes exactly where the player is.

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

// buildTierLadder computes the strategy ladder for one tier from the composer
// ledger (metadata only — no probing). All lighter sources are kept — none is
// discarded — but ordered by distance to the tier's target bitrate so
// degradation is gradual. Example with t0=100 and target=80: available 95,
// 89, 79, 67 orders as 79, 89, 67, 95 — the closest to the 20% lighter ideal
// first, falling back through the next closest if it fails. This avoids a
// single large jump to a very low quality when a gentle step would suffice.
// The transcode candidate is interleaved by the same distance metric, with
// score as tie-break so a professional encode at similar bitrate wins over a
// realtime transcode.
func buildTierLadder(state *playbackState, tier int) []*tierStrategy {
	if tier <= 0 || tier >= tierCount || state.tier0Prepared == nil || state.composer == nil {
		return nil
	}
	t0 := state.tier0Prepared
	t0Key := t0.plan.Video.SourceKey()
	audioKey := t0.plan.Audio.SourceKey()
	if state.active != nil && state.active.prepared != nil {
		audioKey = state.active.prepared.plan.Audio.SourceKey()
	}
	if audioKey == "" || audioKey == t0Key {
		audioKey = ""
	}
	target := state.tierBudgets[tier]
	if target <= 0 {
		target = state.tierBudgets[0] * 8 / 10
		if target <= 0 {
			target = 8_000_000
		}
	}
	t0Bits := streamBandwidth(t0.plan.Video)

	type ranked struct {
		s    *tierStrategy
		dist int64
	}
	var ranked_list []ranked

	// Transcode candidate for this tier.
	tc := transcodeSpecFor(tier, t0.videoHeight)
	tcBits := int64(tc.MaxRateKbps) * 1000
	tcScore := int(float64(analyzer.VideoScore(t0.plan.Video)) * transcodeQuality)
	tcDist := tcBits - target
	if tcDist < 0 {
		tcDist = -tcDist
	}
	ranked_list = append(ranked_list, ranked{
		s: &tierStrategy{
			kind:      stratTranscode,
			transcode: tc,
			score:     tcScore,
			estBits:   tcBits,
			height:    tc.Height,
			desc:      fmt.Sprintf("transcode primary -> %dp@%dk", tc.Height, tc.MaxRateKbps),
		},
		dist: tcDist,
	})

	// All lighter existing sources (strictly below t0), paired with the
	// current dub whenever possible. None discarded — ordered by proximity
	// to target. Sources already marked failed are skipped (ledger is live).
	for _, v := range state.composer.videos {
		if v.failed || v.stream.SourceKey() == t0Key {
			continue
		}
		est := streamBandwidth(v.stream)
		if est >= t0Bits {
			continue
		}
		if est <= 0 {
			continue
		}
		score := analyzer.VideoScore(v.stream)
		if score <= 0 {
			continue
		}
		pair := (*sourceState)(nil)
		if audioKey != "" {
			pair = state.composer.sourceByKey(audioKey)
		}
		if pair == nil || pair.failed {
			for _, a := range state.composer.audios {
				if !a.failed {
					pair = a
					break
				}
			}
		}
		if pair == nil {
			continue
		}
		dist := est - target
		if dist < 0 {
			dist = -dist
		}
		ranked_list = append(ranked_list, ranked{
			s: &tierStrategy{
				kind:    stratSource,
				video:   v,
				audio:   pair,
				score:   score,
				estBits: est,
				height:  resolutionHeight(v.stream.Parsed.Resolution),
				desc:    fmt.Sprintf("source video#%d (%s %s)", v.videoPos, v.stream.Parsed.Resolution, v.stream.Parsed.Quality),
			},
			dist: dist,
		})
	}

	// Sort by distance to target (closest first), tie-break by quality
	// so among equally distant bitrates the better encode wins.
	for i := 1; i < len(ranked_list); i++ {
		for j := i; j > 0; j-- {
			a, b := ranked_list[j-1], ranked_list[j]
			swap := false
			if strategyUnderBudget(b.s, target) != strategyUnderBudget(a.s, target) {
				swap = strategyUnderBudget(b.s, target)
			} else if b.dist < a.dist {
				swap = true
			} else if b.dist == a.dist && b.s.score > a.s.score {
				swap = true
			}
			if !swap {
				break
			}
			ranked_list[j-1], ranked_list[j] = ranked_list[j], ranked_list[j-1]
		}
	}
	ladder := make([]*tierStrategy, 0, len(ranked_list))
	for _, r := range ranked_list {
		ladder = append(ladder, r.s)
	}
	return ladder
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
		clone := *t0
		clone.transcode = s.transcode
		clone.videoBitrate = float64(s.transcode.MaxRateKbps) * 1000
		if t0.videoWidth > 0 && t0.videoHeight > 0 {
			clone.videoWidth = t0.videoWidth * s.transcode.Height / t0.videoHeight
			clone.videoHeight = s.transcode.Height
		}
		return &clone, nil
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

// ensureTier activates a tier lazily: the first request for a tier's segment
// or playlist switches the active generation to that tier's ladder winner at
// the player's position.
func (m *Muxer) ensureTier(job *model.MuxJob, state *playbackState, tier, atSegment int) {
	state.mu.Lock()
	if state.closed || state.tierBusy || state.activeTier == tier || state.tier0Prepared == nil {
		state.mu.Unlock()
		return
	}
	state.tierBusy = true
	state.tierWait = make(chan struct{})
	state.mu.Unlock()

	go m.runTierSwitch(job, state, tier, atSegment)
}

// runTierSwitch walks the tier's ladder at the player's position, falling
// through to deeper tiers' ladders when a whole tier is exhausted. The
// resulting generation serves the requesting tier's namespace regardless of
// which ladder ultimately supplied it.
func (m *Muxer) runTierSwitch(job *model.MuxJob, state *playbackState, tier, atSegment int) {
	start := time.Now()
	winner, fromTier, err := m.launchTierStrategy(job, state, tier, atSegment)

	state.mu.Lock()
	wait := state.tierWait
	old := state.active
	if err == nil && !state.closed {
		state.active = winner
		state.all = append(state.all, winner)
		state.activeTier = tier
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
	state.tierWait = nil
	state.mu.Unlock()

	if wait != nil {
		close(wait)
	}
	if err != nil {
		log.Printf("mux: tier %d switch failed after %s: %v", tier, time.Since(start).Round(time.Millisecond), err)
		return
	}
	if old != nil && old != winner {
		// Grace-cancel the abandoned tier's session: the player may bounce
		// between tiers for a few seconds while its ABR settles.
		go m.cancelAfterGrace(old, 20*time.Second, winner)
	}
	log.Printf("mux: tier %d -> %s (ladder tier %d) at segment %d in %s",
		tier, strategyDesc(winner.prepared), fromTier, atSegment, time.Since(start).Round(time.Millisecond))
	go m.monitorGeneration(job, state, winner)
}

// launchTierStrategy tries every rung of the tier's ladder, then deeper
// tiers' ladders, returning the first generation that produces a segment.
func (m *Muxer) launchTierStrategy(job *model.MuxJob, state *playbackState, tier, atSegment int) (*generation, int, error) {
	var lastErr error
	for t := tier; t < tierCount; t++ {
		ladder := buildTierLadder(state, t)
		for _, s := range ladder {
			ctx, cancel := context.WithTimeout(state.ctx, m.policy.StartupTimeout)
			prepared, err := m.prepareTierStrategy(ctx, job, state, s)
			if err != nil {
				cancel()
				lastErr = err
				log.Printf("mux: tier %d strategy %s failed to prepare: %v", tier, s.desc, err)
				continue
			}
			startTime := float64(atSegment-state.filmBase) * ffmpeg.SegDuration()
			if startTime < 0 {
				startTime = 0
			}
			gen, err := m.launchGeneration(job, state, 1000+t, prepared, tier, atSegment, startTime, m.policy.TierSwitchBuffer)
			cancel()
			if err != nil {
				lastErr = err
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

// cancelAfterGrace cancels a superseded session after the grace period unless
// the player came back to it (it is the active generation again).
func (m *Muxer) cancelAfterGrace(gen *generation, grace time.Duration, replacement *generation) {
	time.Sleep(grace)
	if gen == replacement {
		return
	}
	gen.session.Cancel()
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
