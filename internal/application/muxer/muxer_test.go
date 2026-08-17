package muxer

import (
	"os"
	"path/filepath"
	"strings"
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
	first := tracker.observe(ffmpeg.ProgressSample{At: start.Add(4 * time.Second), OutTime: 2 * time.Second})
	if first.downgrade {
		t.Fatal("one slow window must not request downgrade")
	}
	if first.realtime >= policy.MinRealtime {
		t.Fatalf("first realtime = %.2f, want below %.2f", first.realtime, policy.MinRealtime)
	}

	second := tracker.observe(ffmpeg.ProgressSample{At: start.Add(8 * time.Second), OutTime: 3 * time.Second})
	if !second.downgrade {
		t.Fatalf("two slow windows should request downgrade; realtime %.2f", second.realtime)
	}
}

func TestHealthTrackerHealthyWindowResetsSlowState(t *testing.T) {
	policy := defaultPolicy()
	tracker := newHealthTracker(policy)
	start := time.Unix(200, 0)

	tracker.observe(ffmpeg.ProgressSample{At: start, OutTime: time.Second})
	tracker.observe(ffmpeg.ProgressSample{At: start.Add(4 * time.Second), OutTime: 2 * time.Second})
	healthy := tracker.observe(ffmpeg.ProgressSample{At: start.Add(8 * time.Second), OutTime: 10 * time.Second})
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

func TestBuildVodPlaylistPreservesFilmSequence(t *testing.T) {
	data, ok := buildVodPlaylist(10, 11)
	if !ok {
		t.Fatal("buildVodPlaylist() returned false")
	}
	playlist := string(data)
	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-MEDIA-SEQUENCE:11",
		"#EXT-X-DISCONTINUITY",
		"seg_00011.ts",
		"seg_00012.ts",
		"seg_00013.ts",
		"#EXT-X-ENDLIST",
	} {
		if !strings.Contains(playlist, want) {
			t.Fatalf("playlist missing %q: %s", want, playlist)
		}
	}
}

func TestComputeEqualLengthSegmentsUsesShortFinalSegment(t *testing.T) {
	segments := computeEqualLengthSegments(4, 10)
	want := []float64{4, 4, 2}
	if len(segments) != len(want) {
		t.Fatalf("segments length = %d, want %d", len(segments), len(want))
	}
	for i := range want {
		if segments[i] != want[i] {
			t.Fatalf("segments[%d] = %v, want %v", i, segments[i], want[i])
		}
	}
}

func TestPlaceholderFilmSequenceUsesLastCommonAdvertisedSegment(t *testing.T) {
	dir := t.TempDir()
	for _, media := range []string{"video", "audio"} {
		if err := os.MkdirAll(filepath.Join(dir, media), 0755); err != nil {
			t.Fatal(err)
		}
	}
	video := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4,\nseg_00000.ts\n#EXTINF:4,\nseg_00001.ts\n"
	audio := video + "#EXTINF:4,\nseg_00002.ts\n"
	if err := os.WriteFile(filepath.Join(dir, "video", "video.m3u8"), []byte(video), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audio", "audio.m3u8"), []byte(audio), 0644); err != nil {
		t.Fatal(err)
	}
	if got := placeholderFilmSequence(&generation{dir: dir}); got != 2 {
		t.Fatalf("film sequence = %d, want 2", got)
	}
}

func TestSynchronizedPlaceholderPlaylistUsesCommonWindow(t *testing.T) {
	dir := t.TempDir()
	for _, media := range []string{"video", "audio"} {
		if err := os.MkdirAll(filepath.Join(dir, media), 0755); err != nil {
			t.Fatal(err)
		}
	}
	video := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4,\nseg_00000.ts\n#EXTINF:4,\nseg_00001.ts\n"
	audio := video + "#EXTINF:4,\nseg_00002.ts\n"
	if err := os.WriteFile(filepath.Join(dir, "video", "video.m3u8"), []byte(video), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audio", "audio.m3u8"), []byte(audio), 0644); err != nil {
		t.Fatal(err)
	}
	state := &playbackState{placeholder: &generation{dir: dir}}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}
	data, ok := mux.PlaceholderVideoPlaylist(job)
	if !ok {
		t.Fatal("PlaceholderVideoPlaylist() returned false")
	}
	playlist := string(data)
	if strings.Contains(playlist, "seg_00002.ts") {
		t.Fatalf("video playlist exposed audio-only segment: %s", playlist)
	}
	if !strings.Contains(playlist, "seg_00001.ts") {
		t.Fatalf("video playlist omitted common segment: %s", playlist)
	}
}

func TestIsForwardSeekDistinguishesBufferingFromSeek(t *testing.T) {
	// Pre-buffering: max grows incrementally (2,3,4,5...), so a request just
	// ahead of the max is NOT a seek — wait for the encoder.
	if isForwardSeek(3, 11, 2, 0) {
		t.Fatal("sequential pre-buffering must not be a forward seek")
	}
	// A large jump beyond the previous max is a real seek.
	if !isForwardSeek(5, 600, 3, 0) {
		t.Fatal("large jump must be a forward seek")
	}
	// A request ahead of the max but within the jump threshold is buffering.
	if isForwardSeek(5, 22, 3, 0) {
		t.Fatal("modest jump must not be a forward seek")
	}
	// Never seek before the segment the encoder has produced exists.
	if isForwardSeek(-1, 600, 0, 0) {
		t.Fatal("no prior max must not be a forward seek")
	}
}

func TestEstimatedBandwidthScalesWithResolution(t *testing.T) {
	plan := model.PlaybackPlan{
		Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "2160p", Encode: "HEVC"}},
	}
	if got := plan.EstimatedBandwidth(); got != 16_666_666 {
		t.Fatalf("2160p HEVC bandwidth = %d, want 16666666", got)
	}
	plan.Video.Parsed = model.ParsedFile{Resolution: "1080p", Encode: "AVC"}
	if got := plan.EstimatedBandwidth(); got != 8_000_000 {
		t.Fatalf("1080p AVC bandwidth = %d, want 8000000", got)
	}
}

func TestEstimatedBandwidthPrefersAdvertisedBitrate(t *testing.T) {
	plan := model.PlaybackPlan{
		Video: model.CollectedStream{
			Parsed:       model.ParsedFile{Resolution: "2160p", Encode: "HEVC"},
			VideoBitrate: 62_400_000,
		},
	}
	if got := plan.EstimatedBandwidth(); got != 62_400_000 {
		t.Fatalf("advertised bitrate not preferred: got %d, want 62400000", got)
	}
}

func TestMasterPlaylistAdvertisesActivePlanUnderItsIndex(t *testing.T) {
	state := &playbackState{
		active:       &generation{dir: t.TempDir(), planIndex: 2},
		variantPlans: make(map[int]int),
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{
		ID: "job",
		Plans: []model.PlaybackPlan{
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "2160p"}}},
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "1080p"}}},
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "720p"}}},
		},
	}
	data, ok := mux.MasterPlaylist(job)
	if !ok {
		t.Fatal("MasterPlaylist() returned false")
	}
	playlist := string(data)
	// The active plan (2) must be advertised as v0 (first variant), with its
	// resolution visible. variantPlans must map v0 -> plan 2.
	if !strings.Contains(playlist, "RESOLUTION=720p") || !strings.Contains(playlist, "v0/video/video.m3u8") {
		t.Fatalf("master did not advertise active plan 2 as v0: %s", playlist)
	}
	if state.variantPlans[0] != 2 {
		t.Fatalf("variantPlans[0] = %d, want 2", state.variantPlans[0])
	}
}

func TestMasterPlaylistOmitsAlreadyFailedPlansAboveActive(t *testing.T) {
	state := &playbackState{
		active:       &generation{dir: t.TempDir(), planIndex: 0},
		variantPlans: make(map[int]int),
		failedPlans:  map[int]bool{1: true, 2: true},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{
		ID: "job",
		Plans: []model.PlaybackPlan{
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "2160p"}}},
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "1080p"}}},
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "720p"}}},
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "480p"}}},
		},
	}
	data, ok := mux.MasterPlaylist(job)
	if !ok {
		t.Fatal("MasterPlaylist() returned false")
	}
	playlist := string(data)
	// Active plan 0 (2160p) is v0; plans 1 and 2 failed validation and must
	// be omitted; only plan 3 (480p) follows.
	if !strings.Contains(playlist, "RESOLUTION=2160p") {
		t.Fatalf("active plan 0 missing: %s", playlist)
	}
	if strings.Contains(playlist, "RESOLUTION=1080p") || strings.Contains(playlist, "RESOLUTION=720p") {
		t.Fatalf("failed plans were advertised: %s", playlist)
	}
	if !strings.Contains(playlist, "RESOLUTION=480p") {
		t.Fatalf("valid plan below active missing: %s", playlist)
	}
}

func TestMasterPlaylistAdvertisesVariants(t *testing.T) {
	state := &playbackState{
		active:       &generation{dir: t.TempDir(), planIndex: 0},
		variantPlans: make(map[int]int),
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{
		ID: "job",
		Plans: []model.PlaybackPlan{
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "2160p"}}},
			{Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "1080p"}}},
		},
	}
	data, ok := mux.MasterPlaylist(job)
	if !ok {
		t.Fatal("MasterPlaylist() returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "v0/video/video.m3u8") || !strings.Contains(playlist, "v1/video/video.m3u8") {
		t.Fatalf("master missing variants: %s", playlist)
	}
}

func TestFilmSequenceMapsCutoverAndRetainsEarlierPlaceholderSegments(t *testing.T) {
	filmDir := t.TempDir()
	placeholderDir := t.TempDir()
	for _, dir := range []string{filmDir, placeholderDir} {
		if err := os.MkdirAll(filepath.Join(dir, "video"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	film := filepath.Join(filmDir, "video", "seg_00000.ts")
	placeholder := filepath.Join(placeholderDir, "video", "seg_00001.ts")
	if err := os.WriteFile(film, []byte("film"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placeholder, []byte("placeholder"), 0644); err != nil {
		t.Fatal(err)
	}
	state := &playbackState{
		active:             &generation{dir: filmDir},
		retiredPlaceholder: &generation{dir: placeholderDir},
		filmSequence:       2,
		filmDuration:       120,
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	if path := mux.SegmentPath(job, 2); path != film {
		t.Fatalf("cutover segment path = %q, want %q", path, film)
	}
	if path := mux.SegmentPath(job, 1); path != placeholder {
		t.Fatalf("retired placeholder path = %q, want %q", path, placeholder)
	}
}
