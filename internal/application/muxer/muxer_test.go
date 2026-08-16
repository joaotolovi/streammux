package muxer

import (
	"testing"
	"time"

	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/constants"
	"github.com/streammux/streammux/internal/domain/model"
)

func TestHealthTrackerRequiresTwoSlowNonOverlappingWindows(t *testing.T) {
	policy := defaultPolicy()
	tracker := newHealthTracker(policy)
	start := time.Unix(100, 0)

	if decision := tracker.observe(ffmpeg.ProgressSample{At: start, OutTime: time.Second}); decision.downgrade {
		t.Fatal("first sample must not request downgrade")
	}
	first := tracker.observe(ffmpeg.ProgressSample{At: start.Add(10 * time.Second), OutTime: 5 * time.Second})
	if first.downgrade {
		t.Fatal("one slow window must not request downgrade")
	}
	if first.realtime >= policy.MinRealtime {
		t.Fatalf("first realtime = %.2f, want below %.2f", first.realtime, policy.MinRealtime)
	}

	second := tracker.observe(ffmpeg.ProgressSample{At: start.Add(22 * time.Second), OutTime: 9 * time.Second})
	if !second.downgrade {
		t.Fatalf("two slow windows should request downgrade; realtime %.2f", second.realtime)
	}
}

func TestHealthTrackerHealthyWindowResetsSlowState(t *testing.T) {
	policy := defaultPolicy()
	tracker := newHealthTracker(policy)
	start := time.Unix(200, 0)

	tracker.observe(ffmpeg.ProgressSample{At: start, OutTime: time.Second})
	tracker.observe(ffmpeg.ProgressSample{At: start.Add(10 * time.Second), OutTime: 5 * time.Second})
	healthy := tracker.observe(ffmpeg.ProgressSample{At: start.Add(22 * time.Second), OutTime: 25 * time.Second})
	if healthy.downgrade || healthy.realtime < 1 {
		t.Fatalf("healthy window = %+v", healthy)
	}
}

func TestTargetAudioTrackUsesSingleTrackOfDubbedSource(t *testing.T) {
	// A source identified as dubbed with exactly one audio track is used as-is,
	// no language verification needed.
	tracks := []ffmpeg.AudioTrack{
		{Index: 2, Language: ""},
	}
	source := model.CollectedStream{
		AddonLanguage: "Portuguese (Brazil)",
		IsDubbed:      true,
		Language:      "Portuguese (Brazil)",
	}
	if got := targetAudioTrack(tracks, "Portuguese (Brazil)", source); got != 2 {
		t.Fatalf("targetAudioTrack() = %d, want 2 (single dubbed track)", got)
	}
}

func TestTargetAudioTrackStrictRejectsUndMultiaudio(t *testing.T) {
	// Strict mode: an untagged `und` track in a multiaudio source is NOT picked
	// immediately — that is reserved for the lenient last-resort pass.
	tracks := []ffmpeg.AudioTrack{
		{Index: 0, Language: "eng"},
		{Index: 1, Language: "und"},
		{Index: 2, Language: "spa"},
	}
	source := model.CollectedStream{
		AddonLanguage: "Portuguese (Brazil)",
		IsDubbed:      true,
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, false); got != -1 {
		t.Fatalf("strict targetAudioTrack() = %d, want -1", got)
	}
}

func TestTargetAudioTrackLenientAcceptsUndMultiaudio(t *testing.T) {
	// Lenient last resort: after all strict plans fail, an und/untagged track
	// from a dubbed multiaudio source is accepted.
	tracks := []ffmpeg.AudioTrack{
		{Index: 0, Language: "eng"},
		{Index: 1, Language: "und"},
		{Index: 2, Language: "spa"},
	}
	source := model.CollectedStream{
		AddonLanguage: "Portuguese (Brazil)",
		IsDubbed:      true,
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, true); got != 1 {
		t.Fatalf("lenient targetAudioTrack() = %d, want 1 (und dubbed track)", got)
	}
}

func TestTargetAudioTrackPrefersTaggedPorOverUnd(t *testing.T) {
	// When both a real `por` tag and an `und` track exist, the explicit por
	// track wins even in lenient mode.
	tracks := []ffmpeg.AudioTrack{
		{Index: 0, Language: "und"},
		{Index: 1, Language: "por"},
		{Index: 2, Language: "spa"},
	}
	source := model.CollectedStream{
		AddonLanguage: "Portuguese (Brazil)",
		IsDubbed:      true,
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, true); got != 1 {
		t.Fatalf("targetAudioTrack() = %d, want 1 (por beats und)", got)
	}
}

func TestTargetAudioTrackNeverFallsBackForUnknownMultiaudio(t *testing.T) {
	// Without the addon identifying the source as dubbed, an untagged multiaudio
	// file must not silently pick track zero.
	tracks := []ffmpeg.AudioTrack{
		{Index: 0, Language: "eng"},
		{Index: 1, Language: "spa"},
	}
	source := model.CollectedStream{} // no dubbed signals
	if got := targetAudioTrack(tracks, "Portuguese (Brazil)", source); got != -1 {
		t.Fatalf("targetAudioTrack() = %d, want -1", got)
	}
}

func TestTargetAudioTrackAcceptsSingleUntaggedDubbedTrack(t *testing.T) {
	tracks := []ffmpeg.AudioTrack{{Index: 2}}
	source := model.CollectedStream{AddonLanguage: "Portuguese (Brazil)"}
	if got := targetAudioTrack(tracks, "Portuguese (Brazil)", source); got != 2 {
		t.Fatalf("targetAudioTrack() = %d, want 2", got)
	}
}

func TestCompatibleReleasesRejectsDifferentEditionsAndDurations(t *testing.T) {
	plan := model.PlaybackPlan{
		Video: model.CollectedStream{Parsed: model.ParsedFile{Edition: "Extended"}},
		Audio: model.CollectedStream{Parsed: model.ParsedFile{Edition: "Theatrical"}},
	}
	if err := compatibleReleases(plan, &ffmpeg.ProbeResult{Duration: 7200}, &ffmpeg.ProbeResult{Duration: 7200}, 0.001); err == nil {
		t.Fatal("expected edition mismatch")
	}

	plan.Video.Parsed.Edition = ""
	plan.Audio.Parsed.Edition = ""
	// 7200s * 0.002 = 14.4s, below the 15s cap — a 10s difference must pass.
	if err := compatibleReleases(plan, &ffmpeg.ProbeResult{Duration: 7200}, &ffmpeg.ProbeResult{Duration: 7210}, 0.002); err != nil {
		t.Fatalf("expected 10s difference to pass, got: %v", err)
	}
	// A clearly different release (2 minutes off) still fails.
	if err := compatibleReleases(plan, &ffmpeg.ProbeResult{Duration: 7200}, &ffmpeg.ProbeResult{Duration: 7320}, 0.002); err == nil {
		t.Fatal("expected duration mismatch for 120s difference")
	}
}

func TestCompatibleAudioModeAlwaysCopiesOriginalAudio(t *testing.T) {
	// The original audio is copied regardless of codec. Re-encoding to AAC
	// caused the Android ExoPlayer decoder to reject the stream, so copy is
	// preferred and codec-specific fallback is left to plan selection.
	for _, codec := range []string{"aac", "ac3", "eac3", "dts", "truehd", "flac", "opus", "pcm_s24le"} {
		if got := compatibleAudioMode(codec); got != ffmpeg.AudioModeCopy {
			t.Errorf("compatibleAudioMode(%q) = %q, want copy", codec, got)
		}
	}
}

func TestUniqueAddonsDoesNotQueryBothRoleTwice(t *testing.T) {
	both := model.Addon{ID: "same", ManifestURL: "https://example.test/manifest.json", Role: constants.RoleBoth}
	video := both
	video.Role = constants.RoleVideo
	unique := uniqueAddons([]model.Addon{both, video})
	if len(unique) != 1 {
		t.Fatalf("uniqueAddons() length = %d, want 1", len(unique))
	}
}
