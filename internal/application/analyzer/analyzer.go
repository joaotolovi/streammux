package analyzer

import (
	"sort"

	"github.com/streammux/streammux/internal/domain/constants"
	"github.com/streammux/streammux/internal/domain/model"
)

type Analyzer struct{}

func New() *Analyzer { return &Analyzer{} }

type RankedStream struct {
	Stream     model.CollectedStream
	VideoScore int
	AudioScore int
}

func (a *Analyzer) RankVideo(streams []model.CollectedStream) []RankedStream {
	var ranked []RankedStream
	for _, s := range streams {
		// Video candidates come only from addons configured for video (or both).
		if s.AddonRole != "" && s.AddonRole != constants.RoleVideo && s.AddonRole != constants.RoleBoth {
			continue
		}
		score := scoreVideo(s)
		ranked = append(ranked, RankedStream{Stream: s, VideoScore: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].VideoScore != ranked[j].VideoScore {
			return ranked[i].VideoScore > ranked[j].VideoScore
		}
		return ranked[i].Stream.Size > ranked[j].Stream.Size
	})
	return ranked
}

func (a *Analyzer) RankAudio(streams []model.CollectedStream, targetLanguage string) []RankedStream {
	var ranked []RankedStream
	for _, s := range streams {
		// Audio candidates come only from addons configured for audio (or both).
		if s.AddonRole != "" && s.AddonRole != constants.RoleAudio && s.AddonRole != constants.RoleBoth {
			continue
		}
		if !matchesLanguage(s, targetLanguage) {
			continue
		}
		score := scoreAudio(s)
		ranked = append(ranked, RankedStream{Stream: s, AudioScore: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].AudioScore != ranked[j].AudioScore {
			return ranked[i].AudioScore > ranked[j].AudioScore
		}
		return ranked[i].Stream.Size > ranked[j].Stream.Size
	})
	return ranked
}

func (a *Analyzer) BestVideo(streams []model.CollectedStream) *model.CollectedStream {
	ranked := a.RankVideo(streams)
	if len(ranked) == 0 {
		return nil
	}
	return &ranked[0].Stream
}

func (a *Analyzer) BestAudio(streams []model.CollectedStream, targetLanguage string) *model.CollectedStream {
	ranked := a.RankAudio(streams, targetLanguage)
	if len(ranked) == 0 {
		return nil
	}
	return &ranked[0].Stream
}

func scoreVideo(s model.CollectedStream) int {
	score := 0
	score += getScore(constants.ResolutionScores, s.Parsed.Resolution)
	score += getScore(constants.QualityScores, s.Parsed.Quality)
	score += getScore(constants.EncodeScores, s.Parsed.Encode)
	for _, tag := range s.Parsed.VisualTags {
		score += getScore(constants.VisualTagScores, tag)
	}
	if s.Size > 0 {
		sizeBonus := int(float64(s.Size) / float64(50*1024*1024*1024) * 10)
		if sizeBonus > 10 {
			sizeBonus = 10
		}
		score += sizeBonus
	}
	return score
}

func scoreAudio(s model.CollectedStream) int {
	score := 0
	for _, tag := range s.Parsed.AudioTags {
		score += getScore(constants.AudioTagScores, tag)
	}
	for _, ch := range s.Parsed.AudioChannels {
		score += getScore(constants.AudioChannelScores, ch)
	}
	if s.Size > 0 {
		sizeBonus := int(float64(s.Size) / float64(10*1024*1024*1024) * 10)
		if sizeBonus > 10 {
			sizeBonus = 10
		}
		score += sizeBonus
	}
	return score
}

func matchesLanguage(s model.CollectedStream, target string) bool {
	// The addon's configured language is the strongest signal — the user
	// explicitly marked this addon as a dubbed source.
	if s.AddonLanguage != "" && s.AddonLanguage == target {
		return true
	}
	if s.IsDubbed && (s.Language == target || s.Language == "Dual Audio") {
		return true
	}
	for _, lang := range s.Parsed.Languages {
		if lang == target || lang == "Dual Audio" || lang == "Portuguese" && target == "Portuguese (Brazil)" || lang == "Portuguese (Brazil)" && target == "Portuguese" {
			return true
		}
	}
	return false
}

func getScore(m map[string]int, key string) int {
	if v, ok := m[key]; ok {
		return v
	}
	if v, ok := m["Unknown"]; ok {
		return v
	}
	return 0
}
