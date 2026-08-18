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
	video     *sourceState     // stratSource: the lighter source to switch to
	audio     *sourceState     // stratSource: pairing (usually the current dub)
	transcode *ffmpeg.TranscodeSpec // stratTranscode: params for re-encoding the primary source
	score     int              // expected delivered quality, ranks the rungs
	estBits   int64            // committed/estimated bitrate for ABR metadata
	height    int              // target height for ABR metadata
	desc      string           // human-readable, for logs
}

// tierBudgets returns the per-tier bitrate ceilings derived from the primary
// plan's bitrate. Tier 0 is unbounded (it IS the primary).
func tierBudgets(t0Bits int64) [tierCount]int64 {
	if t0Bits <= 0 {
		t0Bits = 8_000_000
	}
	var b [tierCount]int64
	b[0] = 0 // unlimited
	b[1] = min64(25_000_000, t0Bits*6/10)
	b[2] = min64(10_000_000, t0Bits/4)
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
// ledger (metadata only — no probing). Rungs are ordered by expected delivered
// quality; the first source switch and the transcode are interleaved by score.
func buildTierLadder(state *playbackState, tier int) []*tierStrategy {
	if tier <= 0 || tier >= tierCount || state.tier0Prepared == nil || state.composer == nil {
		return nil
	}
	t0 := state.tier0Prepared
	t0Key := t0.plan.Video.SourceKey()
	audioKey := t0.plan.Audio.SourceKey()
	if audioKey == "" || audioKey == t0Key {
		audioKey = ""
	}
	budget := state.tierBudgets[tier]

	ladder := make([]*tierStrategy, 0, 4)
	tc := transcodeSpecFor(tier, t0.videoHeight)
	tcScore := int(float64(analyzer.VideoScore(t0.plan.Video)) * transcodeQuality)
	ladder = append(ladder, &tierStrategy{
		kind:      stratTranscode,
		transcode: tc,
		score:     tcScore,
		estBits:   int64(tc.MaxRateKbps) * 1000,
		height:    tc.Height,
		desc:      fmt.Sprintf("transcode primary -> %dp@%dk", tc.Height, tc.MaxRateKbps),
	})

	// Lighter existing sources within the tier budget, paired with the
	// current dub whenever possible. Sources already marked failed are
	// skipped (the ledger is live).
	for _, v := range state.composer.videos {
		if v.failed || v.stream.SourceKey() == t0Key {
			continue
		}
		est := streamBandwidth(v.stream)
		if budget > 0 && est > budget {
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
			// Fall back to the composer's best audio for this video; the
			// synthetic composition below resolves tracks itself.
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
		ladder = append(ladder, &tierStrategy{
			kind:    stratSource,
			video:   v,
			audio:   pair,
			score:   score,
			estBits: est,
			height:  resolutionHeight(v.stream.Parsed.Resolution),
			desc:    fmt.Sprintf("source video#%d (%s %s)", v.videoPos, v.stream.Parsed.Resolution, v.stream.Parsed.Quality),
		})
	}

	// Highest expected quality first.
	for i := 1; i < len(ladder); i++ {
		for j := i; j > 0 && ladder[j].score > ladder[j-1].score; j-- {
			ladder[j], ladder[j-1] = ladder[j-1], ladder[j]
		}
	}
	return ladder
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
// playlist from plan metadata only (no probes): tier 0 is the primary plan,
// tiers 1-2 pick the best in-budget source when one exists, else the
// transcode parameters for that tier.
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
	budgets := tierBudgets(t0Bits)

	metas := make([]model.TierMeta, tierCount)
	metas[0] = model.TierMeta{Bandwidth: t0Bits * 12 / 10, Height: t0Height}
	if h := metas[0].Height; h > 0 {
		metas[0].Width = h * 16 / 9
	}

	seen := map[string]bool{primary.Video.SourceKey(): true}
	for tier := 1; tier < tierCount; tier++ {
		budget := budgets[tier]
		best := (*model.CollectedStream)(nil)
		bestScore := -1
		for i := range plans {
			v := &plans[i].Video
			key := v.SourceKey()
			if seen[key] || !plans[i].HasTargetAudio {
				continue
			}
			est := streamBandwidth(*v)
			if budget > 0 && est > budget {
				continue
			}
			if score := analyzer.VideoScore(*v); score > bestScore {
				bestScore = score
				best = v
			}
		}
		if best != nil {
			seen[best.SourceKey()] = true
			est := streamBandwidth(*best)
			h := resolutionHeight(best.Parsed.Resolution)
			metas[tier] = model.TierMeta{Bandwidth: est * 12 / 10, Height: h}
			if h > 0 {
				metas[tier].Width = h * 16 / 9
			}
			continue
		}
		tc := transcodeSpecFor(tier, t0Height)
		metas[tier] = model.TierMeta{
			Bandwidth: int64(tc.MaxRateKbps) * 1000 * 12 / 10,
			Width:     tc.Height * 16 / 9,
			Height:    tc.Height,
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
