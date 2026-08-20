package muxer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	if result.Dubbed.Name != "Oppenheimer • StreamMux MultiAudio" {
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
		details, detailsErr := os.ReadFile(state.detailsCardPath)
		if detailsErr != nil {
			t.Fatalf("read placeholder details: %v", detailsErr)
		}
		if !strings.Contains(string(card), "1080p") || !strings.Contains(string(card), "HDR") || !strings.Contains(string(details), "idiomas") {
			t.Fatalf("card did not receive source metadata: %q", card)
		}
		mux.CleanupJob(job)
	} else {
		t.Fatalf("unexpected mux URL: %q", result.Dubbed.URL)
	}
}
