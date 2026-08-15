package muxer

import (
	"testing"

	"github.com/streammux/streammux/internal/application/analyzer"
	"github.com/streammux/streammux/internal/domain/constants"
	"github.com/streammux/streammux/internal/domain/model"
)

func streamWithURL(url, resolution, quality string) model.Stream {
	return model.Stream{Name: resolution + " " + quality, URL: url}
}

func TestSelectPairPrefersBestVideoAndDubbedAudio(t *testing.T) {
	streams := []model.CollectedStream{
		{
			AddonRole: constants.RoleVideo,
			Stream:    streamWithURL("https://video.example.com/1080p", "1080p", "BluRay"),
			Parsed:    model.ParsedFile{Resolution: "1080p", Quality: "BluRay"},
			Language:  "English",
		},
		{
			AddonRole: constants.RoleVideo,
			Stream:    streamWithURL("https://video.example.com/2160p", "2160p", "BluRay REMUX"),
			Parsed:    model.ParsedFile{Resolution: "2160p", Quality: "BluRay REMUX"},
			Language:  "English",
		},
		{
			AddonRole: constants.RoleAudio,
			Stream:    streamWithURL("https://audio.example.com/dub", "1080p", "BluRay"),
			Parsed:    model.ParsedFile{Resolution: "1080p", Quality: "BluRay", Languages: []string{"Portuguese (Brazil)"}},
			Language:  "Portuguese (Brazil)",
			IsDubbed:  true,
		},
	}

	m := &Muxer{analyzer: analyzer.New()}
	bestVideo, bestAudio := m.selectPair(streams, "Portuguese (Brazil)")

	if bestVideo == nil || bestVideo.Stream.URL != "https://video.example.com/2160p" {
		t.Errorf("expected 2160p video, got %v", bestVideo)
	}
	if bestAudio == nil || bestAudio.Stream.URL != "https://audio.example.com/dub" {
		t.Errorf("expected dubbed audio, got %v", bestAudio)
	}
}

func TestSelectPairSkipsAudioWithoutTargetLanguage(t *testing.T) {
	streams := []model.CollectedStream{
		{
			AddonRole: constants.RoleVideo,
			Stream:    streamWithURL("https://video.example.com", "1080p", "BluRay"),
			Parsed:    model.ParsedFile{Resolution: "1080p", Quality: "BluRay"},
			Language:  "English",
		},
		{
			AddonRole: constants.RoleAudio,
			Stream:    streamWithURL("https://audio.example.com/english", "1080p", "BluRay"),
			Parsed:    model.ParsedFile{Resolution: "1080p", Quality: "BluRay", Languages: []string{"English"}},
			Language:  "English",
		},
	}

	m := &Muxer{analyzer: analyzer.New()}
	bestVideo, bestAudio := m.selectPair(streams, "Portuguese (Brazil)")

	if bestVideo == nil {
		t.Error("expected a video candidate")
	}
	if bestAudio != nil {
		t.Errorf("expected no dubbed audio (only English available), got %v", bestAudio)
	}
}