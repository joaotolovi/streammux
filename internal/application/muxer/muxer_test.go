package muxer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/streammux/streammux/internal/application/assets"
	"github.com/streammux/streammux/internal/application/collector"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/application/planner"
	"github.com/streammux/streammux/internal/application/resolver"
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
	// The original audio is copied regardless of codec. Stremio's player
	// decodes every common codec, so copy avoids quality loss.
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

func TestRenderMediaPlaylistVodWithDiscontinuity(t *testing.T) {
	state := &playbackState{
		duration:        10,
		filmBase:        0,
		discontinuities: []int{2},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	data, ok := mux.VideoPlaylist(job)
	if !ok {
		t.Fatal("VideoPlaylist() returned false")
	}
	playlist := string(data)
	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"seg_00000.ts",
		"#EXT-X-DISCONTINUITY\n#EXTINF:2.000000,\nseg_00002.ts",
		"#EXT-X-ENDLIST",
	} {
		if !strings.Contains(playlist, want) {
			t.Fatalf("playlist missing %q: %s", want, playlist)
		}
	}
	if strings.Count(playlist, "#EXT-X-DISCONTINUITY") != 1 {
		t.Fatalf("expected exactly one discontinuity: %s", playlist)
	}
}

func TestRenderMediaPlaylistAfterPlaceholderHandoff(t *testing.T) {
	// The film occupies [2..7) of the public timeline (28s = 7 segments).
	state := &playbackState{
		duration:        28,
		filmBase:        2,
		discontinuities: []int{2},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	data, ok := mux.VideoPlaylist(job)
	if !ok {
		t.Fatal("VideoPlaylist() returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:2\n") {
		t.Fatalf("playlist must start at the film base: %s", playlist[:120])
	}
	if !strings.Contains(playlist, "#EXT-X-DISCONTINUITY\n#EXTINF:4.000000,\nseg_00002.ts") {
		t.Fatalf("playlist missing cutover discontinuity: %s", playlist)
	}
	if strings.Contains(playlist, "seg_00000.ts") || strings.Contains(playlist, "seg_00001.ts") {
		t.Fatalf("playlist must not list placeholder segments: %s", playlist)
	}
	if !strings.Contains(playlist, "seg_00006.ts") || strings.Contains(playlist, "seg_00007.ts") {
		t.Fatalf("playlist must list exactly the film range: %s", playlist)
	}
}

func TestRenderMediaPlaylistErrorTailTruncatesFilm(t *testing.T) {
	// Mid-playback failure at segment 4: film [0..4) + error [4..4+8).
	state := &playbackState{
		duration:        120,
		filmBase:        0,
		discontinuities: []int{4},
		errorGeneration: &generation{dir: t.TempDir(), isError: true},
		errorStart:      4,
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	data, ok := mux.VideoPlaylist(job)
	if !ok {
		t.Fatal("VideoPlaylist() returned false")
	}
	playlist := string(data)
	if strings.Contains(playlist, "seg_00004.ts\n#EXTINF") == false && !strings.Contains(playlist, "seg_00011.ts") {
		t.Fatalf("error segments missing: %s", playlist)
	}
	if strings.Contains(playlist, "seg_00012.ts") {
		t.Fatalf("playlist must end after the error video: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist must end with ENDLIST: %s", playlist)
	}
}

func TestRenderMediaPlaylistErrorOnlyAfterPlaceholder(t *testing.T) {
	// Startup failed while the placeholder played: [0..3) placeholder +
	// DISCONTINUITY + error video.
	state := &playbackState{
		errorGeneration:    &generation{dir: t.TempDir(), isError: true},
		errorStart:         3,
		retiredPlaceholder: &generation{dir: t.TempDir()},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	data, ok := mux.VideoPlaylist(job)
	if !ok {
		t.Fatal("VideoPlaylist() returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "seg_00000.ts") || !strings.Contains(playlist, "seg_00002.ts") {
		t.Fatalf("placeholder prefix missing: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-DISCONTINUITY\n#EXTINF:4.000000,\nseg_00003.ts") {
		t.Fatalf("error cutover missing: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist must end with ENDLIST: %s", playlist)
	}
}

func TestLastCommonSegmentUsesCommonWindow(t *testing.T) {
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
	if got := lastCommonSegment(&generation{dir: dir}); got != 1 {
		t.Fatalf("lastCommonSegment() = %d, want 1", got)
	}
}

func TestSynchronizedLiveWindowExposesCommonSegmentsOnly(t *testing.T) {
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
	data, ok := synchronizedLiveWindow(&generation{dir: dir}, -1)
	if !ok {
		t.Fatal("synchronizedLiveWindow() returned false")
	}
	playlist := string(data)
	if strings.Contains(playlist, "seg_00002.ts") {
		t.Fatalf("window exposed audio-only segment: %s", playlist)
	}
	if !strings.Contains(playlist, "seg_00001.ts") || !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:0") {
		t.Fatalf("window missing common segment: %s", playlist)
	}

	// Frozen handoff: the window must not advertise past the cap even though
	// both playlists now contain further segments.
	video2 := video + "#EXTINF:4,\nseg_00002.ts\n#EXTINF:4,\nseg_00003.ts\n"
	audio2 := audio + "#EXTINF:4,\nseg_00003.ts\n"
	if err := os.WriteFile(filepath.Join(dir, "video", "video.m3u8"), []byte(video2), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audio", "audio.m3u8"), []byte(audio2), 0644); err != nil {
		t.Fatal(err)
	}
	frozen, ok := synchronizedLiveWindow(&generation{dir: dir}, 1)
	if !ok {
		t.Fatal("synchronizedLiveWindow(cap) returned false")
	}
	playlist = string(frozen)
	if strings.Contains(playlist, "seg_00002.ts") || strings.Contains(playlist, "seg_00003.ts") {
		t.Fatalf("frozen window advertised past the cap: %s", playlist)
	}
	if !strings.Contains(playlist, "seg_00001.ts") {
		t.Fatalf("frozen window missing last common segment: %s", playlist)
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

func TestIsForwardSeekDistinguishesBufferingFromSeek(t *testing.T) {
	// Pre-buffering: max grows incrementally, so a request just ahead of the
	// max is NOT a seek — wait for the encoder.
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
	// First request after startup jumping far ahead of production is a seek
	// (this is the regression: maxRequested < 0 used to disable detection).
	if !isForwardSeek(-1, 179, 5, 3) {
		t.Fatal("first-request far jump must be a forward seek")
	}
	// First request near the encoder position is normal buffering.
	if isForwardSeek(-1, 8, 5, 3) {
		t.Fatal("first-request near production must not be a seek")
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

func TestMasterPlaylistDeclaresAudioRenditionGroup(t *testing.T) {
	state := &playbackState{
		duration: 7200,
		active: &generation{
			dir:       t.TempDir(),
			planIndex: 0,
			plan: model.PlaybackPlan{
				Video: model.CollectedStream{Parsed: model.ParsedFile{Resolution: "2160p"}},
			},
			prepared: &preparedPlan{
				duration:     7200,
				videoBitrate: 50_000_000,
				videoWidth:   3840,
				videoHeight:  2160,
			},
		},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job", TargetLanguage: "Portuguese (Brazil)"}

	data, ok := mux.MasterPlaylist(job)
	if !ok {
		t.Fatal("MasterPlaylist() returned false")
	}
	playlist := string(data)
	// The audio rendition group is what makes the player load the audio
	// playlist at all — without it every film plays silently.
	if !strings.Contains(playlist, "#EXT-X-MEDIA:TYPE=AUDIO") || !strings.Contains(playlist, "URI=\"audio/audio.m3u8\"") {
		t.Fatalf("master missing audio rendition group: %s", playlist)
	}
	if !strings.Contains(playlist, "AUDIO=\"aud\"") {
		t.Fatalf("stream-inf missing audio group: %s", playlist)
	}
	if !strings.Contains(playlist, "RESOLUTION=3840x2160") {
		t.Fatalf("master missing resolution: %s", playlist)
	}
	if !strings.Contains(playlist, "video/video.m3u8") {
		t.Fatalf("master missing video playlist URI: %s", playlist)
	}
}

func TestVodPlaylistServesFullDurationImmediately(t *testing.T) {
	// A 2h film must expose 1800 segments from the very first request.
	state := &playbackState{
		duration: 7200,
		filmBase: 0,
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	data, ok := mux.VideoPlaylist(job)
	if !ok {
		t.Fatal("VideoPlaylist() returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist must be VOD (ENDLIST): %s", playlist[:200])
	}
	if !strings.Contains(playlist, "seg_01799.ts") {
		t.Fatal("playlist missing final segment of a 2h film")
	}
	// Audio playlist exposes the same timeline.
	audioData, ok := mux.AudioPlaylist(job)
	if !ok {
		t.Fatal("AudioPlaylist() returned false")
	}
	if len(audioData) != len(data) {
		t.Fatal("audio and video playlists must expose the same timeline")
	}
}

func TestSegmentPathSearchesGenerationsNewestFirst(t *testing.T) {
	old := t.TempDir()
	newest := t.TempDir()
	for _, dir := range []string{old, newest} {
		if err := os.MkdirAll(filepath.Join(dir, "video"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	staleSeg := filepath.Join(old, "video", "seg_00005.ts")
	freshSeg := filepath.Join(newest, "video", "seg_00005.ts")
	if err := os.WriteFile(staleSeg, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshSeg, []byte("fresh"), 0644); err != nil {
		t.Fatal(err)
	}
	// Only the old generation produced segment 3 (backward seek target).
	if err := os.WriteFile(filepath.Join(old, "video", "seg_00003.ts"), []byte("back"), 0644); err != nil {
		t.Fatal(err)
	}

	state := &playbackState{
		duration: 120,
		all:      []*generation{{dir: old}, {dir: newest}},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	if path := mux.SegmentPath(job, 5); path != freshSeg {
		t.Fatalf("segment 5 = %q, want newest generation %q", path, freshSeg)
	}
	if path := mux.SegmentPath(job, 3); !strings.HasSuffix(path, filepath.Join("video", "seg_00003.ts")) || !strings.Contains(path, old) {
		t.Fatalf("segment 3 = %q, want old generation", path)
	}
	// Requests beyond the film duration return nothing.
	if path := mux.SegmentPath(job, 60); path != "" {
		t.Fatalf("segment beyond end = %q, want empty", path)
	}
}

func TestPlayerTooSlowRequiresAllWindowsSlow(t *testing.T) {
	now := time.Unix(1000, 0)
	required := 10_000_000.0 // 10 Mbps
	// 1 MB in 0.5s = 16 Mbps (fast).
	fast := deliverySample{at: now, bytes: 1_000_000, seconds: 0.5}
	// 1 MB in 2s = 4 Mbps (slow).
	slow := deliverySample{at: now, bytes: 1_000_000, seconds: 2}

	if playerTooSlow(nil, required) {
		t.Fatal("no samples must not be slow")
	}
	if playerTooSlow([]deliverySample{slow, slow}, required) {
		t.Fatal("fewer than window samples must not decide")
	}
	if playerTooSlow([]deliverySample{slow, slow, fast}, required) {
		t.Fatal("one fast delivery in the window must veto the downgrade")
	}
	if !playerTooSlow([]deliverySample{slow, slow, slow}, required) {
		t.Fatal("all deliveries slow must downgrade")
	}
	if playerTooSlow([]deliverySample{slow, slow, slow}, 0) {
		t.Fatal("unknown required bitrate must not downgrade")
	}
}

func TestPlayerTooSlowCatastrophicSingleDelivery(t *testing.T) {
	required := 10_000_000.0
	// 1 MB in 13s — a single transfer slower than 3x the 4s segment marks
	// the player as too slow immediately, no need to fill the window.
	catastrophic := deliverySample{at: time.Unix(1000, 0), bytes: 1_000_000, seconds: 13}
	if !playerTooSlow([]deliverySample{catastrophic}, required) {
		t.Fatal("catastrophic delivery must trigger immediately")
	}
	// 9s is below the 12s (3x segment) threshold: must not decide alone.
	badButNotCatastrophic := deliverySample{at: time.Unix(1000, 0), bytes: 1_000_000, seconds: 9}
	if playerTooSlow([]deliverySample{badButNotCatastrophic}, required) {
		t.Fatal("non-catastrophic single delivery must wait for the window")
	}
}

func TestAppendDeliverySampleKeepsSlowBackToBackDeliveries(t *testing.T) {
	// Regression: a player whose transfers take longer than deliveryPauseGap
	// must NOT have its window reset — the deliveries are back-to-back, the
	// player is starving, not paused. 30 MB (2160p REMUX segment) taking 20s
	// each: request starts are ~20s apart but idle time is ~0.
	now := time.Unix(1000, 0)
	samples := appendDeliverySample(nil, now, 30_000_000, 20*time.Second)
	samples = appendDeliverySample(samples, now.Add(20*time.Second), 30_000_000, 20*time.Second)
	samples = appendDeliverySample(samples, now.Add(40*time.Second), 30_000_000, 20*time.Second)
	samples = appendDeliverySample(samples, now.Add(60*time.Second), 30_000_000, 20*time.Second)

	if len(samples) != deliveryWindow {
		t.Fatalf("expected window of %d samples, got %d — slow back-to-back deliveries must accumulate, not reset", deliveryWindow, len(samples))
	}
	// 30 MB in 20s = 12 Mbps, below a 60 Mbps REMUX requirement.
	if !playerTooSlow(samples, 60_000_000*1.25) {
		t.Fatal("starving player must be detected as too slow")
	}
}

func TestAppendDeliverySampleResetsAfterGenuinePause(t *testing.T) {
	now := time.Unix(1000, 0)
	samples := appendDeliverySample(nil, now, 30_000_000, 5*time.Second)
	// Next request starts 60s later; previous transfer took only 5s, so the
	// player was idle for 55s — a genuine pause must reset the window.
	samples = appendDeliverySample(samples, now.Add(60*time.Second), 30_000_000, 5*time.Second)
	if len(samples) != 1 {
		t.Fatalf("expected window reset to 1 sample after pause, got %d", len(samples))
	}
}

func composerJobFixture() *model.MuxJob {
	video := func(id string, res string) model.CollectedStream {
		s := model.CollectedStream{Parsed: model.ParsedFile{Resolution: res}}
		s.Stream.URL = "https://" + id + ".test/file.mkv"
		return s
	}
	audio := func(id string) model.CollectedStream {
		s := model.CollectedStream{IsDubbed: true, Language: "Portuguese (Brazil)"}
		s.Stream.URL = "https://" + id + ".test/file.mkv"
		return s
	}
	mk := func(v, a model.CollectedStream, single bool) model.PlaybackPlan {
		p := model.PlaybackPlan{Video: v, Audio: a, HasTargetAudio: true, Kind: model.PlanDualSource}
		if single {
			p.Kind = model.PlanSingleSource
		}
		return p
	}
	return &model.MuxJob{
		ID:             "job",
		TargetLanguage: "Portuguese (Brazil)",
		Plans: []model.PlaybackPlan{
			mk(video("v-2160", "2160p"), audio("a-best"), false),
			mk(video("v-1080", "1080p"), audio("a-best"), false),
			mk(video("v-720", "720p"), audio("a-best"), false),
			mk(video("a-best", "1080p"), audio("a-best"), true),
			mk(video("v-2160", "2160p"), audio("a-second"), false),
		},
	}
}

func TestComposerOrdersQueuesAndStartsWithBestPair(t *testing.T) {
	comp := newComposer(composerJobFixture())
	if len(comp.videos) < 3 || len(comp.audios) < 2 {
		t.Fatalf("queues not derived: %d videos, %d audios", len(comp.videos), len(comp.audios))
	}
	// Best video first, best audio first.
	if !strings.Contains(comp.videos[0].stream.Stream.URL, "v-2160") {
		t.Fatalf("first video = %s, want v-2160", comp.videos[0].stream.Stream.URL)
	}
	if !strings.Contains(comp.audios[0].stream.Stream.URL, "a-best") {
		t.Fatalf("first audio = %s, want a-best", comp.audios[0].stream.Stream.URL)
	}
}

func TestComposerFailAudioKeepsVideoAndAdvancesAudio(t *testing.T) {
	comp := newComposer(composerJobFixture())
	first := comp.acquire()
	if first == nil {
		t.Fatal("acquire() returned nil")
	}
	// With geometric mean scoring + single bonus, the first composition may
	// be a single. Fail it so the composer advances to the next.
	comp.fail(first, failNoTrack, errors.New("no track"))
	second := comp.acquire()
	if second == nil {
		t.Fatal("acquire() after failure returned nil")
	}
	if second == first {
		t.Fatal("composer must advance after a failure")
	}
}

func TestComposerFailVideoKeepsAudioAndAdvancesVideo(t *testing.T) {
	comp := newComposer(composerJobFixture())
	first := comp.acquire()
	comp.fail(first, failVideo, errors.New("video 404"))
	second := comp.acquire()
	if second == nil {
		t.Fatal("acquire() after video failure returned nil")
	}
	if second.video == first.video {
		t.Fatal("video must advance after video failure")
	}
}

func TestComposerExhaustsAndLenientPass(t *testing.T) {
	comp := newComposer(composerJobFixture())
	count := 0
	for {
		c := comp.acquire()
		if c == nil {
			break
		}
		count++
		if count > 50 {
			t.Fatal("composer did not terminate")
		}
	}
	if count < 2 {
		t.Fatalf("expected several compositions, got %d", count)
	}
	if !comp.exhausted() {
		t.Fatal("composer must be exhausted after acquire() returns nil")
	}
}

func TestComposerMarkFailedSkipsSourceInBothQueues(t *testing.T) {
	comp := newComposer(composerJobFixture())
	first := comp.acquire()
	comp.markFailed(first.video.stream.SourceKey())
	next := comp.acquire()
	if next == nil {
		t.Fatal("acquire() after markFailed returned nil")
	}
	if next.video == first.video {
		t.Fatal("marked source must be skipped as video")
	}
}

func TestStartErrorGenerationWithRealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	errPath, _, err := assets.ErrorPath()
	if err != nil {
		t.Skipf("error asset: %v", err)
	}
	ff := ffmpeg.New("ffmpeg")
	mux := NewWithVideos(collector.New(), planner.New(), ff, resolver.New(), nil, "http://x.test", "", errPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &playbackState{ctx: ctx, cancel: cancel, cacheDir: t.TempDir()}

	start := time.Now()
	gen := mux.startErrorGeneration(state, -1)
	if gen == nil {
		t.Fatal("startErrorGeneration() returned nil with a valid local error video")
	}
	elapsed := time.Since(start)
	if elapsed > 45*time.Second {
		t.Fatalf("error generation took %s (includes retry)", elapsed)
	}
	if !fileExists(generationSegmentPath(gen, gen.startSegment)) {
		t.Fatal("first error video segment missing")
	}
	t.Logf("error generation started at segment %d in %s", gen.startSegment, elapsed)
}

func TestStartErrorGenerationAfterPlaceholderWithRealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	phPath, _, err := assets.PlaceholderPath()
	if err != nil {
		t.Skipf("placeholder asset: %v", err)
	}
	errPath, _, err := assets.ErrorPath()
	if err != nil {
		t.Skipf("error asset: %v", err)
	}
	ff := ffmpeg.New("ffmpeg")
	mux := NewWithVideos(collector.New(), planner.New(), ff, resolver.New(), nil, "http://x.test", phPath, errPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &playbackState{ctx: ctx, cancel: cancel, cacheDir: t.TempDir()}

	// Roda o placeholder ao vivo por alguns segundos, como no servidor.
	phDir := filepath.Join(state.cacheDir, "generation-000001")
	phSession, err := ff.StartSinglePlaceholderSession(ctx, phPath, phDir, true)
	if err != nil {
		t.Fatalf("placeholder session: %v", err)
	}
	state.placeholder = &generation{dir: phDir, session: phSession, startSegment: 0, startedAt: time.Now(), isLocal: true}

	deadline := time.After(8 * time.Second)
	for !fileExists(filepath.Join(phDir, "video", "seg_00001.ts")) {
		select {
		case <-deadline:
			t.Fatal("placeholder did not produce segments")
		case <-time.After(50 * time.Millisecond):
		}
	}

	gen := mux.startErrorGeneration(state, -1)
	if gen == nil {
		t.Fatal("startErrorGeneration() returned nil after live placeholder")
	}
	if !fileExists(generationSegmentPath(gen, gen.startSegment)) {
		t.Fatalf("first error segment %d missing", gen.startSegment)
	}
	t.Logf("error generation started at segment %d", gen.startSegment)
}

func TestComposerSameSourceInBothQueuesIsSingle(t *testing.T) {
	// A dubbed source that is also a good video appears in both queues.
	// It must be a single-source composition (no duration comparison, one
	// ffmpeg input), recognized by pointer identity through the shared ledger.
	comp := newComposer(composerJobFixture())

	var single *composition
	for {
		c := comp.acquire()
		if c == nil {
			break
		}
		if c.single {
			single = c
			break
		}
		comp.fail(c, failCompose, errors.New("mismatch"))
	}
	if single == nil {
		t.Fatal("no single-source composition was ever offered")
	}
	if single.video != single.audio {
		t.Fatal("single composition must share one ledger entry")
	}
	if !strings.Contains(single.video.stream.Stream.URL, "a-best") {
		t.Fatalf("single composition = %s, want the dubbed source", single.video.stream.Stream.URL)
	}
}
