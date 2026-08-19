package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/domain/constants"
	"github.com/streammux/streammux/internal/domain/model"
)

const (
	maxVideoCandidates = 5
	maxAudioCandidates = 3
	maxDubbedPlans     = 12
)

// Planner builds deterministic playback attempts without probing or modifying
// the collected streams.
type Planner struct{}

func New() *Planner { return &Planner{} }

// VideoCandidates returns every deduplicated collected source for the lazy ABR
// ladder. This is metadata only; resolution and playback remain lazy.
func (p *Planner) VideoCandidates(streams []model.CollectedStream, targetLanguage string) []model.CollectedStream {
	candidates := scoreStreams(deduplicate(streams, targetLanguage))
	sort.Slice(candidates, func(i, j int) bool { return videoCandidateLess(candidates[i], candidates[j]) })
	out := make([]model.CollectedStream, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.stream)
	}
	return out
}

type candidate struct {
	stream     model.CollectedStream
	videoScore int
	audioScore int
}

// Build returns playback plans in attempt order. Target-audio plans always
// precede single-source subtitled recovery plans.
func (p *Planner) Build(streams []model.CollectedStream, targetLanguage string) []model.PlaybackPlan {
	candidates := scoreStreams(deduplicate(streams, targetLanguage))
	if len(candidates) == 0 {
		return nil
	}

	plans := make([]model.PlaybackPlan, 0, len(candidates)+maxDubbedPlans)
	seen := make(map[string]struct{}, len(candidates)+maxDubbedPlans)

	targetAudio := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if analyzer.MatchesLanguage(c.stream, targetLanguage) {
			targetAudio = append(targetAudio, c)
		}
	}

	if len(targetAudio) > 0 {
		primaryVideo := bestCandidate(candidates, primaryVideoLess)
		primaryAudio := bestCandidate(targetAudio, primaryAudioLess)
		primary := makeDubbedPlan(primaryVideo, primaryAudio)
		appendUnique(&plans, seen, primary)

		videoCandidates := limitedCandidates(candidates, maxVideoCandidates, videoCandidateLess, primaryVideo)
		audioCandidates := limitedCandidates(targetAudio, maxAudioCandidates, primaryAudioLess, primaryAudio)

		fallbacks := make([]model.PlaybackPlan, 0, len(targetAudio)+len(videoCandidates)*len(audioCandidates))
		fallbackSeen := map[string]struct{}{planIdentity(primary): {}}

		// Every target-audio source remains usable by itself. In particular, an
		// addon marked as audio may still carry perfectly valid video.
		for _, audio := range targetAudio {
			plan := makeDubbedPlan(audio, audio)
			appendUnique(&fallbacks, fallbackSeen, plan)
		}

		for _, video := range videoCandidates {
			for _, audio := range audioCandidates {
				if video.stream.SourceKey() == audio.stream.SourceKey() {
					continue
				}
				plan := makeDubbedPlan(video, audio)
				appendUnique(&fallbacks, fallbackSeen, plan)
			}
		}

		sort.Slice(fallbacks, func(i, j int) bool {
			return dubbedFallbackLess(fallbacks[i], fallbacks[j])
		})
		for _, plan := range fallbacks {
			if len(plans) >= maxDubbedPlans {
				break
			}
			appendUnique(&plans, seen, plan)
		}
	}

	subtitled := append([]candidate(nil), candidates...)
	sort.Slice(subtitled, func(i, j int) bool {
		return subtitledCandidateLess(subtitled[i], subtitled[j])
	})
	for _, video := range subtitled {
		appendUnique(&plans, seen, makeSubtitledPlan(video))
	}

	return plans
}

func deduplicate(streams []model.CollectedStream, targetLanguage string) []model.CollectedStream {
	byURL := make(map[string]model.CollectedStream, len(streams))
	for _, stream := range streams {
		if strings.TrimSpace(stream.Stream.URL) == "" && strings.TrimSpace(stream.Stream.InfoHash) == "" {
			continue
		}
		key := stream.SourceKey()
		current, exists := byURL[key]
		if !exists || betterRepresentation(stream, current, targetLanguage) {
			byURL[key] = stream
		}
	}

	unique := make([]model.CollectedStream, 0, len(byURL))
	for _, stream := range byURL {
		unique = append(unique, stream)
	}
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].SourceKey() < unique[j].SourceKey()
	})
	return unique
}

func betterRepresentation(left, right model.CollectedStream, targetLanguage string) bool {
	leftVideo, rightVideo := analyzer.VideoScore(left), analyzer.VideoScore(right)
	if leftVideo != rightVideo {
		return leftVideo > rightVideo
	}
	leftAudio, rightAudio := analyzer.AudioScore(left), analyzer.AudioScore(right)
	if leftAudio != rightAudio {
		return leftAudio > rightAudio
	}
	leftTarget := analyzer.MatchesLanguage(left, targetLanguage)
	rightTarget := analyzer.MatchesLanguage(right, targetLanguage)
	if leftTarget != rightTarget {
		return leftTarget
	}
	if roleCapacity(left.AddonRole) != roleCapacity(right.AddonRole) {
		return roleCapacity(left.AddonRole) > roleCapacity(right.AddonRole)
	}
	if left.Size != right.Size {
		return left.Size > right.Size
	}
	if left.Stream.Size != right.Stream.Size {
		return left.Stream.Size > right.Stream.Size
	}
	return representationKey(left) < representationKey(right)
}

func scoreStreams(streams []model.CollectedStream) []candidate {
	result := make([]candidate, 0, len(streams))
	for _, stream := range streams {
		result = append(result, candidate{
			stream:     stream,
			videoScore: analyzer.VideoScore(stream),
			audioScore: analyzer.AudioScore(stream),
		})
	}
	return result
}

func bestCandidate(candidates []candidate, less func(candidate, candidate) bool) candidate {
	best := candidates[0]
	for _, current := range candidates[1:] {
		if less(current, best) {
			best = current
		}
	}
	return best
}

func limitedCandidates(candidates []candidate, limit int, less func(candidate, candidate) bool, required candidate) []candidate {
	ranked := append([]candidate(nil), candidates...)
	sort.Slice(ranked, func(i, j int) bool { return less(ranked[i], ranked[j]) })
	if len(ranked) <= limit {
		return ranked
	}

	ranked = ranked[:limit]
	for _, current := range ranked {
		if current.stream.SourceKey() == required.stream.SourceKey() {
			return ranked
		}
	}

	// Keep the primary source available to combinations without exceeding the
	// advertised candidate bound.
	ranked[len(ranked)-1] = required
	sort.Slice(ranked, func(i, j int) bool { return less(ranked[i], ranked[j]) })
	return ranked
}

func primaryVideoLess(left, right candidate) bool {
	if videoRoleRank(left.stream.AddonRole) != videoRoleRank(right.stream.AddonRole) {
		return videoRoleRank(left.stream.AddonRole) < videoRoleRank(right.stream.AddonRole)
	}
	return videoCandidateLess(left, right)
}

func videoCandidateLess(left, right candidate) bool {
	if left.videoScore != right.videoScore {
		return left.videoScore > right.videoScore
	}
	if videoRoleRank(left.stream.AddonRole) != videoRoleRank(right.stream.AddonRole) {
		return videoRoleRank(left.stream.AddonRole) < videoRoleRank(right.stream.AddonRole)
	}
	if left.audioScore != right.audioScore {
		return left.audioScore > right.audioScore
	}
	return left.stream.SourceKey() < right.stream.SourceKey()
}

func primaryAudioLess(left, right candidate) bool {
	if audioRoleRank(left.stream.AddonRole) != audioRoleRank(right.stream.AddonRole) {
		return audioRoleRank(left.stream.AddonRole) < audioRoleRank(right.stream.AddonRole)
	}
	if left.audioScore != right.audioScore {
		return left.audioScore > right.audioScore
	}
	if left.videoScore != right.videoScore {
		return left.videoScore > right.videoScore
	}
	return left.stream.SourceKey() < right.stream.SourceKey()
}

func dubbedFallbackLess(left, right model.PlaybackPlan) bool {
	if left.VideoScore != right.VideoScore {
		return left.VideoScore > right.VideoScore
	}
	leftSingle := left.Kind == model.PlanSingleSource
	rightSingle := right.Kind == model.PlanSingleSource
	if leftSingle != rightSingle {
		return leftSingle
	}
	if videoRoleRank(left.Video.AddonRole) != videoRoleRank(right.Video.AddonRole) {
		return videoRoleRank(left.Video.AddonRole) < videoRoleRank(right.Video.AddonRole)
	}
	if audioRoleRank(left.Audio.AddonRole) != audioRoleRank(right.Audio.AddonRole) {
		return audioRoleRank(left.Audio.AddonRole) < audioRoleRank(right.Audio.AddonRole)
	}
	if left.AudioScore != right.AudioScore {
		return left.AudioScore > right.AudioScore
	}
	if left.Video.SourceKey() != right.Video.SourceKey() {
		return left.Video.SourceKey() < right.Video.SourceKey()
	}
	if planAudioKey(left) != planAudioKey(right) {
		return planAudioKey(left) < planAudioKey(right)
	}
	return left.Kind < right.Kind
}

func subtitledCandidateLess(left, right candidate) bool {
	if videoRoleRank(left.stream.AddonRole) != videoRoleRank(right.stream.AddonRole) {
		return videoRoleRank(left.stream.AddonRole) < videoRoleRank(right.stream.AddonRole)
	}
	if left.videoScore != right.videoScore {
		return left.videoScore > right.videoScore
	}
	return left.stream.SourceKey() < right.stream.SourceKey()
}

func makeDubbedPlan(video, audio candidate) model.PlaybackPlan {
	kind := model.PlanDualSource
	if video.stream.SourceKey() == audio.stream.SourceKey() {
		kind = model.PlanSingleSource
	}
	plan := model.PlaybackPlan{
		Kind:           kind,
		Video:          video.stream,
		Audio:          audio.stream,
		HasTargetAudio: true,
		VideoScore:     video.videoScore,
		AudioScore:     audio.audioScore,
	}
	plan.ID = makePlanID(plan.Kind, plan.Video.SourceKey(), plan.Audio.SourceKey())
	return plan
}

func makeSubtitledPlan(video candidate) model.PlaybackPlan {
	plan := model.PlaybackPlan{
		Kind:       model.PlanSubtitledFallback,
		Video:      video.stream,
		VideoScore: video.videoScore,
	}
	plan.ID = makePlanID(plan.Kind, plan.Video.SourceKey(), plan.Video.SourceKey())
	return plan
}

func appendUnique(plans *[]model.PlaybackPlan, seen map[string]struct{}, plan model.PlaybackPlan) {
	key := planIdentity(plan)
	if _, exists := seen[key]; exists {
		return
	}
	seen[key] = struct{}{}
	*plans = append(*plans, plan)
}

func planIdentity(plan model.PlaybackPlan) string {
	return string(plan.Kind) + "\x00" + plan.Video.SourceKey() + "\x00" + planAudioKey(plan)
}

func planAudioKey(plan model.PlaybackPlan) string {
	if plan.Audio.Stream.URL == "" && plan.Audio.Stream.InfoHash == "" {
		return plan.Video.SourceKey()
	}
	return plan.Audio.SourceKey()
}

func makePlanID(kind model.PlaybackPlanKind, videoURL, audioURL string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + videoURL + "\x00" + audioURL))
	return string(kind) + ":" + hex.EncodeToString(sum[:])
}

func videoRoleRank(role string) int {
	switch role {
	case constants.RoleVideo, constants.RoleBoth:
		return 0
	case "":
		return 1
	default:
		return 2
	}
}

func audioRoleRank(role string) int {
	switch role {
	case constants.RoleAudio, constants.RoleBoth:
		return 0
	case "":
		return 1
	default:
		return 2
	}
}

func roleCapacity(role string) int {
	switch role {
	case constants.RoleBoth:
		return 2
	case constants.RoleVideo, constants.RoleAudio:
		return 1
	default:
		return 0
	}
}

func representationKey(stream model.CollectedStream) string {
	encoded, err := json.Marshal(stream)
	if err == nil {
		return string(encoded)
	}
	return strings.Join([]string{
		stream.Stream.URL,
		stream.AddonRole,
		stream.AddonID,
		stream.AddonName,
		stream.AddonLanguage,
		stream.Language,
		stream.Stream.Name,
		stream.Stream.Title,
		stream.Stream.Description,
		stream.Stream.InfoHash,
	}, "\x00")
}
