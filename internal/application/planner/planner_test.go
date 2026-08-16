package planner

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/streammux/streammux/internal/domain/constants"
	"github.com/streammux/streammux/internal/domain/model"
)

const targetLanguage = "Portuguese (Brazil)"

func TestBuildCanonicalOrderKeepsDubbed4KSingleBefore1080Dual(t *testing.T) {
	subtitled4K := collected("https://video.example/4k", constants.RoleVideo, "2160p", "BluRay REMUX", false)
	dubbed4K := collected("https://dubbed.example/4k", constants.RoleAudio, "2160p", "BluRay REMUX", true)
	video1080 := collected("https://video.example/1080", constants.RoleVideo, "1080p", "BluRay", false)

	plans := New().Build([]model.CollectedStream{video1080, dubbed4K, subtitled4K}, targetLanguage)

	if len(plans) < 3 {
		t.Fatalf("expected at least three plans, got %d", len(plans))
	}
	assertPlan(t, plans[0], model.PlanDualSource, subtitled4K.Stream.URL, dubbed4K.Stream.URL)
	assertPlan(t, plans[1], model.PlanSingleSource, dubbed4K.Stream.URL, dubbed4K.Stream.URL)

	var dual1080Index = -1
	for i, plan := range plans {
		if plan.Kind == model.PlanDualSource && plan.VideoURL() == video1080.Stream.URL && plan.AudioURL() == dubbed4K.Stream.URL {
			dual1080Index = i
			break
		}
	}
	if dual1080Index == -1 {
		t.Fatal("expected a 1080p dual-source fallback")
	}
	if dual1080Index <= 1 {
		t.Fatalf("expected dubbed 4K single-source before 1080p dual-source, dual index %d", dual1080Index)
	}

	seenSubtitled := false
	for _, plan := range plans {
		if plan.Kind == model.PlanSubtitledFallback {
			seenSubtitled = true
			if plan.HasTargetAudio {
				t.Errorf("subtitled plan %q reports target audio", plan.ID)
			}
			continue
		}
		if seenSubtitled {
			t.Fatalf("dubbed plan %q appears after subtitled fallbacks", plan.ID)
		}
		if !plan.HasTargetAudio {
			t.Errorf("dubbed plan %q does not report target audio", plan.ID)
		}
	}
}

func TestBuildUsesOppositeRolesAsFallbacks(t *testing.T) {
	videoWithAudioRole := collected("https://opposite.example/video", constants.RoleAudio, "2160p", "BluRay REMUX", false)
	audioWithVideoRole := collected("https://opposite.example/audio", constants.RoleVideo, "1080p", "WEB-DL", true)

	plans := New().Build([]model.CollectedStream{videoWithAudioRole, audioWithVideoRole}, targetLanguage)

	if !containsPlan(plans, model.PlanDualSource, videoWithAudioRole.Stream.URL, audioWithVideoRole.Stream.URL) {
		t.Fatal("expected dual plan using streams with opposite roles")
	}
	if !containsPlan(plans, model.PlanSingleSource, audioWithVideoRole.Stream.URL, audioWithVideoRole.Stream.URL) {
		t.Fatal("expected target-audio source with video role to remain usable as single-source")
	}
	if !containsPlan(plans, model.PlanSubtitledFallback, videoWithAudioRole.Stream.URL, videoWithAudioRole.Stream.URL) {
		t.Fatal("expected audio-role source to remain available as subtitled video recovery")
	}
}

func TestBuildWithoutTargetAudioReturnsOnlyOrderedSubtitledFallbacks(t *testing.T) {
	video := collected("https://fallback.example/video", constants.RoleVideo, "1080p", "WEB-DL", false)
	unspecified := collected("https://fallback.example/unspecified", "", "2160p", "BluRay REMUX", false)
	audioRole := collected("https://fallback.example/audio-role", constants.RoleAudio, "2160p", "BluRay REMUX", false)

	plans := New().Build([]model.CollectedStream{audioRole, unspecified, video}, targetLanguage)

	if len(plans) != 3 {
		t.Fatalf("expected three subtitled plans, got %d", len(plans))
	}
	wantURLs := []string{video.Stream.URL, unspecified.Stream.URL, audioRole.Stream.URL}
	for i, plan := range plans {
		if plan.Kind != model.PlanSubtitledFallback {
			t.Errorf("plan %d has kind %q, want subtitled fallback", i, plan.Kind)
		}
		if plan.VideoURL() != wantURLs[i] {
			t.Errorf("plan %d URL = %q, want %q", i, plan.VideoURL(), wantURLs[i])
		}
		if plan.HasTargetAudio {
			t.Errorf("plan %d unexpectedly reports target audio", i)
		}
		if plan.Audio.Stream.URL != "" {
			t.Errorf("plan %d should use zero Audio for subtitled fallback", i)
		}
	}
}

func TestBuildIgnoresEmptyURLsAndKeepsBestDuplicateRepresentation(t *testing.T) {
	low := collected("https://duplicate.example/source", constants.RoleAudio, "720p", "WEBRip", false)
	high := collected("https://duplicate.example/source", constants.RoleVideo, "2160p", "BluRay REMUX", false)
	missing := collected("", constants.RoleVideo, "2160p", "BluRay REMUX", false)
	whitespace := collected("   ", constants.RoleVideo, "2160p", "BluRay REMUX", false)

	plans := New().Build([]model.CollectedStream{low, missing, high, whitespace}, targetLanguage)

	if len(plans) != 1 {
		t.Fatalf("expected one deduplicated fallback, got %d", len(plans))
	}
	if plans[0].Video.Parsed.Resolution != "2160p" || plans[0].Video.AddonRole != constants.RoleVideo {
		t.Fatalf("did not preserve best duplicate representation: %+v", plans[0].Video)
	}
}

func TestBuildIsDeterministicRegardlessOfInputOrder(t *testing.T) {
	duplicateLow := collected("https://deterministic.example/dub", constants.RoleAudio, "720p", "WEBRip", true)
	streams := []model.CollectedStream{
		collected("https://deterministic.example/video-1080", constants.RoleVideo, "1080p", "BluRay", false),
		collected("https://deterministic.example/dub", constants.RoleAudio, "2160p", "BluRay REMUX", true),
		collected("https://deterministic.example/video-4k", constants.RoleVideo, "2160p", "BluRay REMUX", false),
		duplicateLow,
		collected("https://deterministic.example/both", constants.RoleBoth, "1080p", "WEB-DL", true),
	}
	reversed := append([]model.CollectedStream(nil), streams...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}

	forwardPlans := New().Build(streams, targetLanguage)
	reversePlans := New().Build(reversed, targetLanguage)
	if !reflect.DeepEqual(forwardPlans, reversePlans) {
		t.Fatalf("plans depend on input order:\nforward: %#v\nreverse: %#v", forwardPlans, reversePlans)
	}

	ids := make(map[string]struct{}, len(forwardPlans))
	for _, plan := range forwardPlans {
		if plan.ID == "" {
			t.Fatal("plan has empty deterministic ID")
		}
		if _, exists := ids[plan.ID]; exists {
			t.Fatalf("duplicate plan ID %q", plan.ID)
		}
		ids[plan.ID] = struct{}{}
	}
}

func TestBuildLimitsDualCandidatesAndDubbedPlans(t *testing.T) {
	streams := make([]model.CollectedStream, 0, 13)
	resolutions := []string{"2160p", "1440p", "1080p", "720p", "576p", "480p", "360p", "240p"}
	for i, resolution := range resolutions {
		streams = append(streams, collected(
			fmt.Sprintf("https://limits.example/video-%02d", i),
			constants.RoleVideo,
			resolution,
			"WEB-DL",
			false,
		))
	}
	for i := 0; i < 5; i++ {
		stream := collected(
			fmt.Sprintf("https://limits.example/audio-%02d", i),
			constants.RoleAudio,
			"1080p",
			"WEB-DL",
			true,
		)
		stream.Parsed.AudioTags = []string{"Atmos"}
		stream.Parsed.AudioChannels = []string{"7.1"}
		streams = append(streams, stream)
	}

	plans := New().Build(streams, targetLanguage)
	dubbedCount := 0
	dualVideos := make(map[string]struct{})
	dualAudios := make(map[string]struct{})
	seenSubtitled := false
	identities := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		identity := fmt.Sprintf("%s\x00%s\x00%s", plan.Kind, plan.VideoURL(), plan.AudioURL())
		if _, exists := identities[identity]; exists {
			t.Fatalf("duplicate plan: %s", identity)
		}
		identities[identity] = struct{}{}

		if plan.Kind == model.PlanSubtitledFallback {
			seenSubtitled = true
			continue
		}
		if seenSubtitled {
			t.Fatal("found dubbed plan after subtitled fallback")
		}
		dubbedCount++
		if plan.Kind == model.PlanDualSource {
			dualVideos[plan.VideoURL()] = struct{}{}
			dualAudios[plan.AudioURL()] = struct{}{}
		}
	}

	if dubbedCount != maxDubbedPlans {
		t.Fatalf("dubbed plan count = %d, want %d", dubbedCount, maxDubbedPlans)
	}
	if len(dualVideos) > maxVideoCandidates {
		t.Fatalf("dual plans use %d video candidates, maximum is %d", len(dualVideos), maxVideoCandidates)
	}
	if len(dualAudios) > maxAudioCandidates {
		t.Fatalf("dual plans use %d audio candidates, maximum is %d", len(dualAudios), maxAudioCandidates)
	}
}

func collected(url, role, resolution, quality string, target bool) model.CollectedStream {
	stream := model.CollectedStream{
		AddonID:   role + "-addon",
		AddonName: role + " addon",
		AddonRole: role,
		Stream: model.Stream{
			Name: resolution + " " + quality,
			URL:  url,
		},
		Parsed: model.ParsedFile{
			Resolution: resolution,
			Quality:    quality,
		},
		Language: "English",
	}
	if target {
		stream.AddonLanguage = targetLanguage
		stream.Language = targetLanguage
		stream.IsDubbed = true
		stream.Parsed.Languages = []string{targetLanguage}
	}
	return stream
}

func assertPlan(t *testing.T, plan model.PlaybackPlan, kind model.PlaybackPlanKind, videoURL, audioURL string) {
	t.Helper()
	if plan.Kind != kind || plan.VideoURL() != videoURL || plan.AudioURL() != audioURL {
		t.Fatalf(
			"plan = (%s, %s, %s), want (%s, %s, %s)",
			plan.Kind,
			plan.VideoURL(),
			plan.AudioURL(),
			kind,
			videoURL,
			audioURL,
		)
	}
}

func containsPlan(plans []model.PlaybackPlan, kind model.PlaybackPlanKind, videoURL, audioURL string) bool {
	for _, plan := range plans {
		if plan.Kind == kind && plan.VideoURL() == videoURL && plan.AudioURL() == audioURL {
			return true
		}
	}
	return false
}
