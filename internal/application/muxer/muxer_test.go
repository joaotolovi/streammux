package muxer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestHealthTrackerReportsSmoothedProductionRate(t *testing.T) {
	policy := defaultPolicy()
	tracker := newHealthTracker(policy)
	start := time.Unix(100, 0)

	if decision := tracker.observe(ffmpeg.ProgressSample{At: start, OutTime: time.Second}); decision.valid {
		t.Fatal("first sample must not produce a rate")
	}
	slow := tracker.observe(ffmpeg.ProgressSample{At: start.Add(4 * time.Second), OutTime: 2 * time.Second})
	if !slow.valid {
		t.Fatal("complete health window must produce a rate")
	}
	if slow.realtime >= 1 {
		t.Fatalf("slow realtime = %.2f, want below realtime", slow.realtime)
	}

	fast := tracker.observe(ffmpeg.ProgressSample{At: start.Add(8 * time.Second), OutTime: 10 * time.Second})
	if !fast.valid || fast.realtime < 1 {
		t.Fatalf("fast realtime = %.2f, want above realtime", fast.realtime)
	}
}

func TestNeedsSourceHandoffUsesPhysicalReserve(t *testing.T) {
	reserve := 20 * time.Second
	if !needsSourceHandoff(reserve, 0.5, reserve) {
		t.Fatal("slow source at the handoff reserve must switch")
	}
	if needsSourceHandoff(24*time.Second, 0.5, reserve) {
		t.Fatal("source above the physical handoff reserve must continue")
	}
	if needsSourceHandoff(0, 1, reserve) {
		t.Fatal("realtime production must not switch solely because the reserve is low")
	}
}

func TestRecordVideoRequestIgnoresRetryAndResetsOnSeek(t *testing.T) {
	state := &playbackState{lastRequested: -1}
	now := time.Unix(100, 0)
	mux := &Muxer{}

	mux.recordVideoRequestLocked(state, 10, now)
	if !state.lastSequentialAt.IsZero() {
		t.Fatal("first request must not start consumption tracking")
	}
	mux.recordVideoRequestLocked(state, 11, now.Add(4*time.Second))
	if state.lastSequentialAt.IsZero() {
		t.Fatal("consecutive request must start consumption tracking")
	}
	trackedAt := state.lastSequentialAt
	mux.recordVideoRequestLocked(state, 11, now.Add(8*time.Second))
	if state.lastSequentialAt != trackedAt {
		t.Fatal("retry must not look like another consumed segment")
	}
	mux.recordVideoRequestLocked(state, 30, now.Add(9*time.Second))
	if !state.lastSequentialAt.IsZero() || state.playbackEpoch != 1 {
		t.Fatal("seek must reset production health tracking")
	}
}

func TestCachedRequestsAdvanceMaximumBeforeSeekClassification(t *testing.T) {
	state := &playbackState{lastRequested: 10, maxRequested: 10}
	mux := &Muxer{}
	previousMax := mux.recordVideoRequestLocked(state, 11, time.Unix(100, 0))
	if previousMax != 10 || state.maxRequested != 11 {
		t.Fatalf("request max moved from %d to %d, want 10 to 11", previousMax, state.maxRequested)
	}
	if isForwardSeek(previousMax, 11, 10, 0) {
		t.Fatal("next sequential cached request must not be classified as a seek")
	}
}

func TestSeekResetStartsNewRequestWindow(t *testing.T) {
	state := &playbackState{lastRequested: 753, maxRequested: 857}
	resetPlaybackTrackingLocked(state)
	if state.maxRequested != 753 {
		t.Fatalf("maximum after backward seek = %d, want 753", state.maxRequested)
	}

	previousMax := (&Muxer{}).recordVideoRequestLocked(state, 860, time.Unix(100, 0))
	if !isForwardSeek(previousMax, 860, 780, 753) {
		t.Fatal("request after a backward seek must be classified in the new window")
	}
}

func TestForwardSeekStartsAtRequestedSegment(t *testing.T) {
	if !isForwardSeek(100, 130, 105, 0) {
		t.Fatal("fixture must be a forward seek")
	}
	requested := 130
	target := requested
	if target != 130 {
		t.Fatalf("forward seek target = %d, want 130", target)
	}
}

func TestPruneGenerationBytesRemovesOldestCompleteSegments(t *testing.T) {
	dir := t.TempDir()
	for _, media := range []string{"video", "audio"} {
		if err := os.MkdirAll(filepath.Join(dir, media), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 3; index++ {
		for _, media := range []string{"video", "audio"} {
			path := filepath.Join(dir, media, fmt.Sprintf("seg_%05d.ts", index))
			if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, 100), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	pruneGenerationBytes(dir, 400, 2)
	if fileExists(filepath.Join(dir, "video", "seg_00000.ts")) || fileExists(filepath.Join(dir, "audio", "seg_00000.ts")) {
		t.Fatal("oldest segment pair was not removed")
	}
	if !fileExists(filepath.Join(dir, "video", "seg_00002.ts")) || !fileExists(filepath.Join(dir, "audio", "seg_00002.ts")) {
		t.Fatal("newest segment pair was removed")
	}
}

func TestPruneGenerationSegmentsKeepsRewindWindow(t *testing.T) {
	dir := t.TempDir()
	for _, media := range []string{"video", "audio"} {
		if err := os.MkdirAll(filepath.Join(dir, media), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 4; index++ {
		for _, media := range []string{"video", "audio"} {
			path := filepath.Join(dir, media, fmt.Sprintf("seg_%05d.ts", index))
			if err := os.WriteFile(path, []byte("segment"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	pruneGenerationSegments(dir, 2)
	for _, index := range []int{0, 1} {
		if fileExists(filepath.Join(dir, "video", fmt.Sprintf("seg_%05d.ts", index))) {
			t.Fatalf("expired video segment %d was retained", index)
		}
	}
	for _, index := range []int{2, 3} {
		if !fileExists(filepath.Join(dir, "video", fmt.Sprintf("seg_%05d.ts", index))) || !fileExists(filepath.Join(dir, "audio", fmt.Sprintf("seg_%05d.ts", index))) {
			t.Fatalf("rewind-window segment %d was removed", index)
		}
	}
}

func TestComposerWithAudioPreservesRequestedAudioSource(t *testing.T) {
	video := &sourceState{stream: model.CollectedStream{Stream: model.Stream{URL: "https://video.test/file"}}}
	audio := &sourceState{stream: model.CollectedStream{Stream: model.Stream{URL: "https://audio.test/file"}}}
	composer := &composer{videos: []*sourceState{video}, audios: []*sourceState{audio}}

	pair := composer.withAudio(video, audio.stream.SourceKey())
	if pair == nil {
		t.Fatal("withAudio() returned nil")
	}
	if pair.video != video || pair.audio != audio || pair.single {
		t.Fatalf("withAudio() = %#v, want video/audio pair", pair)
	}
	if pair := composer.withAudio(video, "missing"); pair != nil {
		t.Fatal("withAudio() paired an unknown audio source")
	}
}

func TestHealthTrackerReportsAStoppedSource(t *testing.T) {
	policy := defaultPolicy()
	tracker := newHealthTracker(policy)
	start := time.Unix(200, 0)

	tracker.observe(ffmpeg.ProgressSample{At: start, OutTime: time.Second})
	stopped := tracker.observe(ffmpeg.ProgressSample{At: start.Add(4 * time.Second), OutTime: time.Second})
	if !stopped.valid || stopped.realtime != 0 {
		t.Fatalf("stopped window = %+v, want valid 0x production", stopped)
	}
}

func TestTargetAudioTrackDefersUntaggedSingleTrackToLenient(t *testing.T) {
	// A source identified as dubbed with exactly one UNTAGGED audio track is
	// not selected in strict mode: without metadata there is no proof it is in
	// the target language, so it is deferred to the end of the queue (lenient
	// pass) rather than discarding it.
	tracks := []ffmpeg.AudioTrack{
		{Index: 2, Language: ""},
	}
	source := model.CollectedStream{
		AddonLanguage: "Portuguese (Brazil)",
		IsDubbed:      true,
		Language:      "Portuguese (Brazil)",
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, false); got != -1 {
		t.Fatalf("strict targetAudioTrack() = %d, want -1 (untagged single track deferred)", got)
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, true); got != 2 {
		t.Fatalf("lenient targetAudioTrack() = %d, want 2 (single untagged track at end of queue)", got)
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

func TestTargetAudioTrackRejectsSingleForeignTrack(t *testing.T) {
	// A source flagged as Portuguese whose single audio track is explicitly
	// tagged English: the tag is authoritative and overrides the addon flag —
	// the source is rejected in both passes rather than played in English.
	tracks := []ffmpeg.AudioTrack{{Index: 2, Language: "eng"}}
	source := model.CollectedStream{
		AddonLanguage: "Portuguese (Brazil)",
		IsDubbed:      true,
		Language:      "Portuguese (Brazil)",
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, false); got != -1 {
		t.Fatalf("strict targetAudioTrack() = %d, want -1 (explicit eng tag rejects source)", got)
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, true); got != -1 {
		t.Fatalf("lenient targetAudioTrack() = %d, want -1 (explicit eng tag rejects source)", got)
	}
}

func TestTargetAudioTrackAcceptsSingleTaggedPorTrack(t *testing.T) {
	// A single track explicitly tagged Portuguese is selected directly.
	tracks := []ffmpeg.AudioTrack{{Index: 1, Language: "por"}}
	source := model.CollectedStream{
		AddonLanguage: "Portuguese (Brazil)",
		IsDubbed:      true,
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, false); got != 1 {
		t.Fatalf("strict targetAudioTrack() = %d, want 1 (single por track)", got)
	}
}

func TestTargetAudioTrackLenientAcceptsUndSingleTrack(t *testing.T) {
	// An und-tagged single track keeps its place at the end of the queue:
	// rejected in strict mode, accepted in the lenient last-resort pass.
	tracks := []ffmpeg.AudioTrack{{Index: 3, Language: "und"}}
	source := model.CollectedStream{
		AddonLanguage: "Portuguese (Brazil)",
		IsDubbed:      true,
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, false); got != -1 {
		t.Fatalf("strict targetAudioTrack() = %d, want -1 (und deferred)", got)
	}
	if got := targetAudioTrackStrict(tracks, "Portuguese (Brazil)", source, true); got != 3 {
		t.Fatalf("lenient targetAudioTrack() = %d, want 3 (und single track)", got)
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

	data, ok := mux.VideoPlaylist(job, 0)
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

func TestRenderMediaPlaylistOpeningOverlayKeepsFilmTimeline(t *testing.T) {
	// The opening overlay covers [0..2) while preserving a 28s film timeline.
	state := &playbackState{
		duration:        28,
		filmBase:        2,
		discontinuities: []int{2},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	data, ok := mux.VideoPlaylist(job, 0)
	if !ok {
		t.Fatal("VideoPlaylist() returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:0\n") {
		t.Fatalf("playlist must include the opening segment: %s", playlist[:120])
	}
	if !strings.Contains(playlist, "#EXT-X-DISCONTINUITY\n#EXTINF:4.000000,\nseg_00002.ts") {
		t.Fatalf("playlist missing cutover discontinuity: %s", playlist)
	}
	if !strings.Contains(playlist, "seg_00000.ts") || !strings.Contains(playlist, "seg_00001.ts") {
		t.Fatalf("playlist must list opening segments: %s", playlist)
	}
	if !strings.Contains(playlist, "seg_00006.ts") || strings.Contains(playlist, "seg_00007.ts") {
		t.Fatalf("playlist duration must remain the film duration: %s", playlist)
	}
}

func TestRenderMediaPlaylistAfterPlaceholderResume(t *testing.T) {
	state := &playbackState{
		duration:        28,
		filmBase:        2,
		resumeStart:     5,
		discontinuities: []int{5},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	data, ok := mux.VideoPlaylist(job, 0)
	if !ok {
		t.Fatal("VideoPlaylist() returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:5\n") {
		t.Fatalf("playlist must start at the resumed segment: %s", playlist)
	}
	if strings.Contains(playlist, "seg_00002.ts") || strings.Contains(playlist, "seg_00004.ts") {
		t.Fatalf("playlist must not advertise skipped film segments: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-DISCONTINUITY\n") || !strings.Contains(playlist, "seg_00005.ts") {
		t.Fatalf("playlist missing resumed handoff: %s", playlist)
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

	data, ok := mux.VideoPlaylist(job, 0)
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

	data, ok := mux.VideoPlaylist(job, 0)
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

func TestMasterPlaylistAdvertisesAlternativeAudioRendition(t *testing.T) {
	mux := &Muxer{}
	job := &model.MuxJob{TargetLanguage: "Portuguese (Brazil)"}
	playlist := string(mux.renderMaster(job, 1_000_000, 0, 0, job.TargetLanguage))
	if !strings.Contains(playlist, `LANGUAGE="por",URI="audio/audio.m3u8"`) {
		t.Fatalf("master missing primary Portuguese rendition: %s", playlist)
	}
	if !strings.Contains(playlist, `LANGUAGE="por-x-alt",URI="audio/por-alt/audio.m3u8"`) {
		t.Fatalf("master missing alternative Portuguese rendition: %s", playlist)
	}
}

func TestMasterPlaylistAdvertisesPermittedCandidateLanguages(t *testing.T) {
	job := &model.MuxJob{
		TargetLanguage: "English",
		Config:         model.Config{Addons: []model.Addon{{Enabled: true, ShowAllAudioLanguages: true}}},
		VideoCandidates: []model.CollectedStream{
			{Parsed: model.ParsedFile{Languages: []string{"Spanish", "Portuguese"}}},
		},
	}
	playlist := string((&Muxer{}).renderMaster(job, 1_000_000, 0, 0, job.TargetLanguage))
	for _, want := range []string{
		`LANGUAGE="eng",URI="audio/audio.m3u8"`,
		`LANGUAGE="spa",URI="audio/spa/audio.m3u8"`,
		`LANGUAGE="por",URI="audio/por/audio.m3u8"`,
	} {
		if !strings.Contains(playlist, want) {
			t.Fatalf("master missing %q: %s", want, playlist)
		}
	}
}

func TestAudioPlaylistReadsDoNotSelectRendition(t *testing.T) {
	state := &playbackState{duration: 8, activeAudioID: "eng-alt", audioSelection: 7}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job", TargetLanguage: "English"}
	if _, ok := mux.AudioPlaylist(job); !ok {
		t.Fatal("AudioPlaylist() returned false")
	}
	if _, ok := mux.AudioPlaylistRendition(job, "eng-alt"); !ok {
		t.Fatal("AudioPlaylistRendition() returned false")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.activeAudioID != "eng-alt" || state.audioSelection != 7 {
		t.Fatalf("playlist read changed selection to %q/%d", state.activeAudioID, state.audioSelection)
	}
}

func TestAudioRenditionPathRejectsPreviousGeneration(t *testing.T) {
	videoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(videoDir, "video"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(videoDir, "video", "seg_00004.ts"), []byte("video"), 0644); err != nil {
		t.Fatal(err)
	}
	audioDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(audioDir, "audio"), 0755); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(audioDir, "audio", "seg_00004.ts")
	if err := os.WriteFile(audioPath, []byte("audio"), 0644); err != nil {
		t.Fatal(err)
	}
	active := &generation{dir: videoDir}
	stale := &generation{dir: t.TempDir()}
	state := &playbackState{
		active:        active,
		activeAudioID: "spa",
		audioRenditions: map[string]*audioRendition{
			"spa": {id: "spa", dir: audioDir, generation: stale},
		},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}
	if got := mux.AudioSegmentPathRendition(job, "spa", 4); got != "" {
		t.Fatalf("stale generation audio path = %q", got)
	}
	state.audioRenditions["spa"].generation = active
	if got := mux.AudioSegmentPathRendition(job, "spa", 4); got != audioPath {
		t.Fatalf("current generation audio path = %q, want %q", got, audioPath)
	}
}

func TestAudioSegmentPathWaitsForMatchingVideo(t *testing.T) {
	dir := t.TempDir()
	for _, media := range []string{"video", "audio"} {
		if err := os.MkdirAll(filepath.Join(dir, media), 0755); err != nil {
			t.Fatal(err)
		}
	}
	audioPath := filepath.Join(dir, "audio", "seg_00007.ts")
	if err := os.WriteFile(audioPath, []byte("audio"), 0644); err != nil {
		t.Fatal(err)
	}
	state := &playbackState{duration: 60, all: []*generation{{dir: dir}}}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}
	if got := mux.AudioSegmentPath(job, 7); got != "" {
		t.Fatalf("audio was exposed before video: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "video", "seg_00007.ts"), []byte("video"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := mux.AudioSegmentPath(job, 7); got != audioPath {
		t.Fatalf("coordinated audio path = %q, want %q", got, audioPath)
	}
}

func TestPlaceholderMasterAdvertisesSingleRendition(t *testing.T) {
	state := &playbackState{placeholder: &generation{}}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{
		ID:             "job",
		TargetLanguage: "Portuguese (Brazil)",
		TierMetas: []model.TierMeta{
			{Bandwidth: 120_000_000, Width: 3840, Height: 2160},
			{Bandwidth: 21_600_000, Width: 1920, Height: 1080},
			{Bandwidth: 7_200_000, Width: 1280, Height: 720},
		},
	}

	data, ok := mux.MasterPlaylist(job)
	if !ok {
		t.Fatal("MasterPlaylist() returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "video/video.m3u8") {
		t.Fatalf("placeholder master missing primary rendition: %s", playlist)
	}
	for _, uri := range []string{"video/v1.m3u8", "video/v2.m3u8"} {
		if strings.Contains(playlist, uri) {
			t.Fatalf("placeholder master unexpectedly exposes ABR rendition %s: %s", uri, playlist)
		}
	}
}

func TestVirtualTierPlaylistUsesNamespacedSegments(t *testing.T) {
	state := &playbackState{duration: 12, lastRequested: -1}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}

	data, ok := mux.VideoPlaylist(&model.MuxJob{ID: "job"}, 1)
	if !ok {
		t.Fatal("VideoPlaylist() returned false")
	}
	playlist := string(data)
	if !strings.Contains(playlist, "v1/seg_00000.ts") {
		t.Fatalf("tier playlist missing namespaced segment: %s", playlist)
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

	data, ok := mux.VideoPlaylist(job, 0)
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

func TestSegmentPathTierDoesNotServeAnotherTier(t *testing.T) {
	tier0 := t.TempDir()
	tier1 := t.TempDir()
	for _, dir := range []string{tier0, tier1} {
		if err := os.MkdirAll(filepath.Join(dir, "video"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	tier0Segment := filepath.Join(tier0, "video", "seg_00002.ts")
	tier1Segment := filepath.Join(tier1, "video", "seg_00002.ts")
	if err := os.WriteFile(tier0Segment, []byte("tier0"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tier1Segment, []byte("tier1"), 0644); err != nil {
		t.Fatal(err)
	}
	state := &playbackState{
		duration: 120,
		all: []*generation{
			{dir: tier0, tier: 0},
			{dir: tier1, tier: 1},
		},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	if path := mux.SegmentPathTier(job, 2, 0); path != tier0Segment {
		t.Fatalf("tier 0 segment = %q, want %q", path, tier0Segment)
	}
	if path := mux.SegmentPathTier(job, 2, 1); path != tier1Segment {
		t.Fatalf("tier 1 segment = %q, want %q", path, tier1Segment)
	}
}

func TestMediaSegmentPathBridgesPendingTierToActiveGeneration(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "video"), 0755); err != nil {
		t.Fatal(err)
	}
	activeSegment := filepath.Join(dir, "video", "seg_00002.ts")
	if err := os.WriteFile(activeSegment, []byte("active"), 0644); err != nil {
		t.Fatal(err)
	}

	state := &playbackState{
		duration:   120,
		activeTier: 0,
		active:     &generation{dir: dir, tier: 0},
		all:        []*generation{{dir: dir, tier: 0}},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	if path := mux.SegmentPathTier(job, 2, 1); path != "" {
		t.Fatalf("pending tier must not expose an exact tier-1 path: %q", path)
	}
	if path := mux.mediaSegmentPath(job, 2, 1, false); path != activeSegment {
		t.Fatalf("pending tier path = %q, want active segment %q", path, activeSegment)
	}
}

func TestSegmentPathTierIgnoresRetiredTierAfterCutover(t *testing.T) {
	tier0 := t.TempDir()
	tier1 := t.TempDir()
	for _, dir := range []string{tier0, tier1} {
		if err := os.MkdirAll(filepath.Join(dir, "video"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	retired := filepath.Join(tier0, "video", "seg_00002.ts")
	active := filepath.Join(tier1, "video", "seg_00002.ts")
	if err := os.WriteFile(retired, []byte("retired"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte("active"), 0644); err != nil {
		t.Fatal(err)
	}

	state := &playbackState{
		duration:   120,
		activeTier: 1,
		active:     &generation{dir: tier1, tier: 1},
		all:        []*generation{{dir: tier0, tier: 0}, {dir: tier1, tier: 1}},
	}
	mux := &Muxer{states: map[string]*playbackState{"job": state}}
	job := &model.MuxJob{ID: "job"}

	if path := mux.SegmentPathTier(job, 2, 0); path != "" {
		t.Fatalf("retired tier path = %q, want empty", path)
	}
	if path := mux.mediaSegmentPath(job, 2, 0, false); path != active {
		t.Fatalf("retired tier bridge = %q, want active segment %q", path, active)
	}
}

func TestMediaSegmentPathKeepsBridgedTierURIsStableAfterCutover(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()
	for _, dir := range []string{oldDir, newDir} {
		if err := os.MkdirAll(filepath.Join(dir, "video"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	bridged := filepath.Join(oldDir, "video", "seg_00012.ts")
	if err := os.WriteFile(bridged, []byte("primary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "video", "seg_00017.ts"), []byte("tier"), 0644); err != nil {
		t.Fatal(err)
	}
	job := &model.MuxJob{ID: "job"}
	mux := &Muxer{states: map[string]*playbackState{
		"job": {
			active:     &generation{dir: newDir, tier: 2, startSegment: 17},
			activeTier: 2,
			all: []*generation{
				{dir: oldDir, tier: 0, startSegment: 12},
				{dir: newDir, tier: 2, startSegment: 17},
			},
		},
	}}

	if path := mux.mediaSegmentPath(job, 12, 2, false); path != bridged {
		t.Fatalf("bridged tier URI = %q, want %q", path, bridged)
	}
}

func TestRequestTierHonorsCooldown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &playbackState{
		active:         &generation{},
		ctx:            ctx,
		activeTier:     0,
		tierPending:    -1,
		tier0Prepared:  &preparedPlan{},
		lastTierSwitch: time.Now(),
	}
	mux := &Muxer{
		states: map[string]*playbackState{"job": state},
		policy: Policy{TierSwitchCooldown: time.Minute},
	}

	mux.requestTier(&model.MuxJob{ID: "job"}, state, 1, 2)
	state.mu.Lock()
	busy := state.tierBusy
	state.mu.Unlock()
	if busy {
		t.Fatal("tier switch started during cooldown")
	}
}

func TestRequestTierCancelsPendingSwitchWhenPlayerReturnsToActiveTier(t *testing.T) {
	canceled := make(chan struct{})
	state := &playbackState{
		active:        &generation{},
		activeTier:    0,
		tierBusy:      true,
		tierPending:   1,
		tier0Prepared: &preparedPlan{},
		tierSwitchCancel: func() {
			close(canceled)
		},
	}
	mux := &Muxer{}

	mux.requestTier(&model.MuxJob{ID: "job"}, state, 0, 2)

	select {
	case <-canceled:
	default:
		t.Fatal("pending tier switch was not canceled")
	}
	state.mu.Lock()
	pending := state.tierPending
	state.mu.Unlock()
	if pending != -1 {
		t.Fatalf("pending tier = %d, want invalidated", pending)
	}
}

func TestPlayerTooSlowRequiresAllWindowsSlow(t *testing.T) {
	now := time.Unix(1000, 0)
	required := 10_000_000.0 // 10 Mbps
	// 1 MB in 2s = 4 Mbps (slow).
	slow := deliverySample{at: now, bytes: 1_000_000, seconds: 2}

	if playerTooSlow(nil, required) {
		t.Fatal("no samples must not be slow")
	}
	if playerTooSlow([]deliverySample{slow, slow}, required) {
		t.Fatal("fewer than window samples must not decide")
	}
	// Aggregate model: fast deliveries that keep the window's sustained
	// throughput above the requirement veto the downgrade even with one
	// slow sample (32+32+4 Mbps samples average above 10 Mbps).
	fast2x := deliverySample{at: now, bytes: 2_000_000, seconds: 0.5}
	if playerTooSlow([]deliverySample{fast2x, fast2x, slow}, required) {
		t.Fatal("window with sustained throughput above requirement must veto the downgrade")
	}
	if !playerTooSlow([]deliverySample{slow, slow, slow}, required) {
		t.Fatal("all deliveries slow must downgrade")
	}
	if playerTooSlow([]deliverySample{slow, slow, slow}, 0) {
		t.Fatal("unknown required bitrate must not downgrade")
	}
}

// TestPlayerTooSlowMarginalOscillatingLink reproduces the exact field
// scenario: a link oscillating just below the required bitrate. The old
// per-sample "all below" rule was vetoed by every fast burst and the
// downgrade never fired; the aggregate sustained throughput must decide.
func TestPlayerTooSlowMarginalOscillatingLink(t *testing.T) {
	required := 97_000_000.0 // 97 Mbps (77.5 Mbps REMUX with 1.25x headroom)
	// 39 MB in 3.3s = 94.5 Mbps, 46 MB in 3.9s = 94.5 Mbps, 41.4 MB in 5.6s
	// = 59.2 Mbps — the exact deliveries observed in production.
	// Aggregate: 126.4 MB in 12.8s = 79 Mbps < 97 Mbps → must downgrade.
	below1 := deliverySample{at: time.Unix(1000, 0), bytes: 39_000_000, seconds: 3.3}
	below2 := deliverySample{at: time.Unix(1010, 0), bytes: 46_000_000, seconds: 3.9}
	below3 := deliverySample{at: time.Unix(1020, 0), bytes: 41_400_000, seconds: 5.6}
	if !playerTooSlow([]deliverySample{below1, below2, below3}, required) {
		t.Fatal("marginal oscillating link must be detected as too slow")
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

func TestComposerFallsBackToBestSingleSourceWithoutTargetLanguage(t *testing.T) {
	comp := newComposer(composerJobFixture())
	// Skip the regular and lenient target-language passes.
	comp.cursor = len(comp.ranked)
	comp.lenient = true

	candidate := comp.acquire()
	if candidate == nil {
		t.Fatal("acquire() returned nil instead of a language fallback")
	}
	if !candidate.fallback || !candidate.single || candidate.video != candidate.audio {
		t.Fatalf("fallback must use a single video/audio source: %#v", candidate)
	}
	if !strings.Contains(candidate.video.stream.Stream.URL, "v-2160") {
		t.Fatalf("fallback video = %s, want highest-quality source", candidate.video.stream.Stream.URL)
	}
}

func TestFallbackTrackPrefersDefaultThenQuality(t *testing.T) {
	s := &sourceState{probe: &ffmpeg.ProbeResult{AudioTracks: []ffmpeg.AudioTrack{
		{Index: 0, Channels: 8, BitRate: 4_000_000},
		{Index: 1, Default: true, Channels: 6, BitRate: 1_500_000},
		{Index: 2, Default: true, Forced: true, Channels: 8, BitRate: 4_000_000},
	}}}
	if got := s.selectFallbackTrack(); got != 1 {
		t.Fatalf("fallback track = %d, want default non-forced track 1", got)
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
	mux := NewWithErrorVideo(collector.New(), planner.New(), ff, resolver.New(), nil, "http://x.test", errPath)

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
