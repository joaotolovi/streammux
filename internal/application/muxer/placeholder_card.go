package muxer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/streammux/streammux/internal/domain/model"
)

// renderPlaceholderCard keeps the overlay useful even before an addon has
// returned anything. Once plans arrive, it adds the best-known source and
// audio facts without pretending that an unprobed stream is guaranteed.
func renderPlaceholderCard(metadata contentMetadata, plans []model.PlaybackPlan, language string) string {
	title := metadata.Title
	if title == "" {
		title = "StreamMux MultiAudio"
	}
	lines := []string{"🎬 " + title}
	var identity []string
	if metadata.Year != "" {
		identity = append(identity, metadata.Year)
	}
	if metadata.Rating != "" {
		identity = append(identity, "⭐ "+metadata.Rating)
	}
	if len(identity) > 0 {
		lines = append(lines, strings.Join(identity, " • "))
	}

	var primary *model.PlaybackPlan
	for i := range plans {
		if plans[i].HasTargetAudio {
			primary = &plans[i]
			break
		}
	}
	if primary == nil && len(plans) > 0 {
		primary = &plans[0]
	}

	if primary != nil {
		var videoFacts []string
		for _, fact := range []string{
			primary.Video.Parsed.Resolution,
			primary.Video.Parsed.Quality,
			primary.Video.Parsed.Encode,
		} {
			if fact != "" {
				videoFacts = append(videoFacts, fact)
			}
		}
		videoFacts = append(videoFacts, primary.Video.Parsed.VisualTags...)
		if len(videoFacts) > 0 {
			lines = append(lines, strings.Join(uniqueCardFacts(videoFacts), " • "))
		}

		audioLanguage := language
		if audioLanguage == "" {
			audioLanguage = primary.Audio.Language
		}
		languages := collectCardLanguages(plans)
		audioLine := "Áudio no seu idioma"
		if audioLanguage != "" {
			audioLine = "Áudio: " + audioLanguage
		}
		if len(languages) > 1 {
			audioLine += fmt.Sprintf(" • +%d idiomas disponíveis", len(languages)-1)
		}
		lines = append(lines, audioLine)
		lines = append(lines, "qualidade máxima • remux automático")
		return strings.Join(lines, "\n")
	}

	lines = append(lines, "Preparando fontes...")
	return strings.Join(lines, "\n")
}

func uniqueCardFacts(facts []string) []string {
	seen := make(map[string]struct{}, len(facts))
	out := make([]string, 0, len(facts))
	for _, fact := range facts {
		fact = strings.TrimSpace(fact)
		if fact == "" {
			continue
		}
		if _, ok := seen[fact]; ok {
			continue
		}
		seen[fact] = struct{}{}
		out = append(out, fact)
	}
	return out
}

func collectCardLanguages(plans []model.PlaybackPlan) []string {
	seen := make(map[string]struct{})
	for _, plan := range plans {
		for _, stream := range []model.CollectedStream{plan.Video, plan.Audio} {
			values := append([]string(nil), stream.Parsed.Languages...)
			if stream.Language != "" {
				values = append(values, stream.Language)
			}
			for _, language := range values {
				language = strings.ToLower(strings.TrimSpace(language))
				if language != "" {
					seen[language] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for language := range seen {
		out = append(out, language)
	}
	sort.Strings(out)
	return out
}

func writePlaceholderCard(path, contents string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".placeholder-card-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(contents + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
