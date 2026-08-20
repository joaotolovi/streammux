package muxer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
	"github.com/streammux/streammux/internal/infrastructure/store"
)

type blockingCollector struct {
	started chan struct{}
	release chan struct{}
	streams []model.CollectedStream
}

func (c *blockingCollector) CollectStreams(ctx context.Context, _ []model.Addon, _, _ string) []model.CollectedStream {
	select {
	case <-c.started:
	default:
		close(c.started)
	}
	select {
	case <-c.release:
		return c.streams
	case <-ctx.Done():
		return nil
	}
}

type fixedPlanner struct {
	plans []model.PlaybackPlan
}

func (p *fixedPlanner) Build([]model.CollectedStream, string) []model.PlaybackPlan {
	return append([]model.PlaybackPlan(nil), p.plans...)
}

func (p *fixedPlanner) VideoCandidates(streams []model.CollectedStream, _ string) []model.CollectedStream {
	return append([]model.CollectedStream(nil), streams...)
}

func TestProcessReturnsBeforeAddonPreparationAndUsesCinemetaIdentity(t *testing.T) {
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meta/movie/tt123.json" {
			t.Fatalf("unexpected Cinemeta path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"name":"Oppenheimer","releaseInfo":"2023","imdbRating":8.6}}`))
	}))
	defer metaServer.Close()

	oldBase := cinemetaBaseURL
	cinemetaBaseURL = metaServer.URL
	defer func() { cinemetaBaseURL = oldBase }()

	video := model.CollectedStream{
		AddonName: "Addon Video",
		Stream:    model.Stream{URL: "https://video.test/movie.mkv"},
		Parsed: model.ParsedFile{
			Resolution: "1080p", Quality: "WEB-DL", Encode: "HEVC",
			VisualTags: []string{"HDR"}, Languages: []string{"English", "Portuguese"},
		},
	}
	audio := video
	audio.AddonName = "Addon Audio"
	audio.Stream = model.Stream{URL: "https://audio.test/movie.mka"}
	audio.Language = "Portuguese"
	plan := model.PlaybackPlan{Kind: model.PlanDualSource, Video: video, Audio: audio, HasTargetAudio: true}

	collector := &blockingCollector{started: make(chan struct{}), release: make(chan struct{}), streams: []model.CollectedStream{video, audio}}
	jobStore := store.NewMemoryStore(time.Hour)
	mux := &Muxer{
		collector:  collector,
		planner:    &fixedPlanner{plans: []model.PlaybackPlan{plan}},
		store:      jobStore,
		states:     make(map[string]*playbackState),
		policy:     defaultPolicy(),
		httpClient: metaServer.Client(),
	}

	cfg := &model.Config{Language: "Portuguese", Addons: []model.Addon{{ID: "addon", Enabled: true}}}
	start := time.Now()
	result, err := mux.Process(context.Background(), cfg, "movie", "tt123")
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Process waited %s for addon preparation", elapsed)
	}
	if result.Dubbed == nil {
		t.Fatal("Process returned no stream")
	}
	if result.Dubbed.Name != "▶ Oppenheimer • StreamMux MultiAudio" {
		t.Fatalf("stream name = %q", result.Dubbed.Name)
	}
	if !strings.Contains(result.Dubbed.Description, "2023 • ★ 8.6") || !strings.Contains(result.Dubbed.Description, "qualidade máxima") {
		t.Fatalf("unexpected stream description: %q", result.Dubbed.Description)
	}

	select {
	case <-collector.started:
	case <-time.After(time.Second):
		t.Fatal("addon worker did not start")
	}
	if jobID := strings.TrimPrefix(result.Dubbed.URL, "/mux/"); strings.Contains(jobID, "/") {
		jobID = strings.TrimSuffix(jobID, "/playlist.m3u8")
		job, ok := jobStore.Get(jobID)
		if !ok {
			t.Fatal("job was not saved")
		}
		state := mux.lookupState(job.ID)
		state.mu.Lock()
		preparationWait := state.preparationWait
		state.mu.Unlock()
		close(collector.release)
		select {
		case <-preparationWait:
		case <-time.After(time.Second):
			t.Fatal("addon preparation did not finish")
		}
		card, readErr := os.ReadFile(state.cardPath)
		if readErr != nil {
			t.Fatalf("read placeholder card: %v", readErr)
		}
		if !strings.Contains(string(card), "1080p") || !strings.Contains(string(card), "HDR") || !strings.Contains(string(card), "idiomas disponíveis") {
			t.Fatalf("card did not receive source metadata: %q", card)
		}
		mux.CleanupJob(job)
	} else {
		t.Fatalf("unexpected mux URL: %q", result.Dubbed.URL)
	}
}

type dynamicPlaceholderFake struct {
	fakeMediaEngine
	specs []ffmpeg.PlaceholderSpec
}

func (f *dynamicPlaceholderFake) StartPlaceholderSession(_ context.Context, spec ffmpeg.PlaceholderSpec) (*ffmpeg.Session, error) {
	f.specs = append(f.specs, spec)
	session := &ffmpeg.Session{}
	session.InitDone()
	go func() {
		_ = os.MkdirAll(filepath.Join(spec.OutputDir, "video"), 0755)
		_ = os.MkdirAll(filepath.Join(spec.OutputDir, "audio"), 0755)
		var video, audio strings.Builder
		video.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		audio.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		for i := spec.StartSegment; i < spec.StartSegment+4; i++ {
			_ = os.WriteFile(filepath.Join(spec.OutputDir, "video", segmentName(i)), []byte("video"), 0644)
			_ = os.WriteFile(filepath.Join(spec.OutputDir, "audio", segmentName(i)), []byte("audio"), 0644)
			video.WriteString("#EXTINF:4,\n" + segmentName(i) + "\n")
			audio.WriteString("#EXTINF:4,\n" + segmentName(i) + "\n")
		}
		_ = os.WriteFile(filepath.Join(spec.OutputDir, "video", "video.m3u8"), []byte(video.String()), 0644)
		_ = os.WriteFile(filepath.Join(spec.OutputDir, "audio", "audio.m3u8"), []byte(audio.String()), 0644)
	}()
	return session, nil
}

func segmentName(segment int) string {
	return fmt.Sprintf("seg_%05d.ts", segment)
}

func TestLatePosterReplacementStartsAtNextPublicSegment(t *testing.T) {
	engine := &dynamicPlaceholderFake{}
	mux := &Muxer{ffmpeg: engine, policy: defaultPolicy(), states: make(map[string]*playbackState)}
	mux.policy.TierSwitchBuffer = 0
	job := &model.MuxJob{ID: "poster-job"}
	oldDir := t.TempDir()
	for _, media := range []string{"video", "audio"} {
		_ = os.MkdirAll(filepath.Join(oldDir, media), 0755)
	}
	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
	for i := 0; i < 3; i++ {
		name := segmentName(i)
		_ = os.WriteFile(filepath.Join(oldDir, "video", name), []byte("v"), 0644)
		_ = os.WriteFile(filepath.Join(oldDir, "audio", name), []byte("a"), 0644)
		playlist.WriteString("#EXTINF:4,\n" + name + "\n")
	}
	_ = os.WriteFile(filepath.Join(oldDir, "video", "video.m3u8"), []byte(playlist.String()), 0644)
	_ = os.WriteFile(filepath.Join(oldDir, "audio", "audio.m3u8"), []byte(playlist.String()), 0644)
	oldSession := &ffmpeg.Session{}
	oldSession.InitDone()
	posterPath := filepath.Join(t.TempDir(), "poster.jpg")
	_ = os.WriteFile(posterPath, []byte("poster"), 0644)
	old := &generation{dir: oldDir, session: oldSession, startSegment: 0, isLocal: true}
	state := &playbackState{ctx: context.Background(), cacheDir: t.TempDir(), placeholder: old, all: []*generation{old}, posterPath: posterPath, tierMetas: placeholderTierMetas()}
	mux.states[job.ID] = state

	mux.swapPlaceholder(job, state, old)
	deadline := time.After(time.Second)
	for {
		state.mu.Lock()
		newGen := state.placeholder
		disc := state.placeholderDiscAt
		hasDisc := state.placeholderHasDisc
		state.mu.Unlock()
		if newGen != old {
			if len(engine.specs) != 1 || engine.specs[0].StartSegment != 3 || engine.specs[0].ImagePath != posterPath {
				t.Fatalf("unexpected late poster spec: %#v", engine.specs)
			}
			if !hasDisc || disc != 3 {
				t.Fatalf("placeholder discontinuity = (%v, %d), want (true, 3)", hasDisc, disc)
			}
			data, ok := mux.renderMediaPlaylist(job, 0)
			if !ok || !strings.Contains(string(data), "#EXT-X-DISCONTINUITY\n#EXTINF:4,\nseg_00003.ts") {
				t.Fatalf("replacement playlist missing cutover discontinuity: %s", data)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("late poster replacement did not complete")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
