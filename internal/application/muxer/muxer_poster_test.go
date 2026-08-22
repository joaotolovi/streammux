package muxer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/domain/model"
)

// fakeMediaEngine is a minimal mediaEngine implementation that lets us
// observe which placeholder session variant is requested.
type fakeMediaEngine struct {
	imageCalls []string
	plainCalls []string
	probeFn    func(context.Context, string) (*ffmpeg.ProbeResult, error)
}

func (f *fakeMediaEngine) Probe(ctx context.Context, url string) (*ffmpeg.ProbeResult, error) {
	if f.probeFn != nil {
		return f.probeFn(ctx, url)
	}
	return &ffmpeg.ProbeResult{}, nil
}

func (f *fakeMediaEngine) StartSession(ctx context.Context, spec ffmpeg.SessionSpec) (*ffmpeg.Session, error) {
	return nil, nil
}

func (f *fakeMediaEngine) StartSinglePlaceholderSession(ctx context.Context, path, outputDir string, realtime bool) (*ffmpeg.Session, error) {
	f.plainCalls = append(f.plainCalls, outputDir)
	return newFakeSession(outputDir), nil
}

func (f *fakeMediaEngine) StartImagePlaceholderSession(ctx context.Context, path, imagePath, outputDir string, realtime bool) (*ffmpeg.Session, error) {
	f.imageCalls = append(f.imageCalls, imagePath)
	return newFakeSession(outputDir), nil
}

func newFakeSession(outputDir string) *ffmpeg.Session {
	s := &ffmpeg.Session{}
	s.InitDone()
	// Simulate segment production by creating a tiny segment file in the output dir.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.MkdirAll(filepath.Join(outputDir, "video"), 0755)
		_ = os.MkdirAll(filepath.Join(outputDir, "audio"), 0755)
		_ = os.WriteFile(filepath.Join(outputDir, "video", "seg_00000.ts"), []byte("segment"), 0644)
		_ = os.WriteFile(filepath.Join(outputDir, "audio", "seg_00000.ts"), []byte("segment"), 0644)
		_ = os.WriteFile(filepath.Join(outputDir, "video", "video.m3u8"), []byte("playlist"), 0644)
		_ = os.WriteFile(filepath.Join(outputDir, "audio", "audio.m3u8"), []byte("playlist"), 0644)
	}()
	return s
}

func (f *fakeMediaEngine) DetectAudioOffset(videoURL, audioURL string, tracks []ffmpeg.AudioTrack, sampleRate int, duration float64) (time.Duration, int, float64, error) {
	return 0, 0, 0, nil
}

func (f *fakeMediaEngine) GenerateInterstitial(ctx context.Context, placeholderPath, outputDir string, duration time.Duration) error {
	_ = os.MkdirAll(outputDir, 0755)
	_ = os.WriteFile(filepath.Join(outputDir, "intro.m3u8"), []byte("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg_00000.ts\n#EXTINF:4.0,\nseg_00001.ts\n#EXT-X-ENDLIST\n"), 0644)
	_ = os.WriteFile(filepath.Join(outputDir, "seg_00000.ts"), []byte("segment"), 0644)
	_ = os.WriteFile(filepath.Join(outputDir, "seg_00001.ts"), []byte("segment"), 0644)
	return nil
}

func waitPlaceholder(t *testing.T, state *playbackState) {
	t.Helper()
	select {
	case <-state.placeholderWait:
	case <-time.After(2 * time.Second):
		state.mu.Lock()
		wait := state.placeholderWait
		state.mu.Unlock()
		if wait != nil {
			t.Fatal("placeholder wait not closed")
		}
	}
}

func TestRunPlaceholderUsesImageWhenPosterExists(t *testing.T) {
	ff := &fakeMediaEngine{}
	mux := &Muxer{
		ffmpeg:          ff,
		placeholderPath: "placeholder.mp4",
		policy:          defaultPolicy(),
		states:          make(map[string]*playbackState),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	posterDir := t.TempDir()
	posterPath := filepath.Join(posterDir, posterFileName)
	_ = os.WriteFile(posterPath, []byte("poster"), 0644)

	job := &model.MuxJob{
		ID:          "job",
		ContentType: "movie",
		ContentID:   "tt0111161",
	}

	state := &playbackState{
		ctx:           ctx,
		cancel:        cancel,
		cacheDir:      t.TempDir(),
		posterPath:    posterPath,
		lastRequested: -1,
		maxRequested:  -1,
	}
	mux.states[job.ID] = state

	go mux.runPlaceholder(job, state)
	waitPlaceholder(t, state)

	if len(ff.imageCalls) != 1 {
		t.Fatalf("expected image placeholder call, got %d", len(ff.imageCalls))
	}
	if len(ff.plainCalls) != 0 {
		t.Fatalf("expected no plain placeholder calls, got %d", len(ff.plainCalls))
	}
	cancel()
}

func TestRunPlaceholderFallsBackToPlainWhenPosterMissing(t *testing.T) {
	ff := &fakeMediaEngine{}
	mux := &Muxer{
		ffmpeg:          ff,
		placeholderPath: "placeholder.mp4",
		policy:          defaultPolicy(),
		states:          make(map[string]*playbackState),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	job := &model.MuxJob{
		ID:          "job",
		ContentType: "movie",
		ContentID:   "tt0111161",
	}

	// posterPath set but file doesn't exist → should fall back to plain.
	state := &playbackState{
		ctx:           ctx,
		cancel:        cancel,
		cacheDir:      t.TempDir(),
		posterPath:    filepath.Join(t.TempDir(), posterFileName),
		lastRequested: -1,
		maxRequested:  -1,
	}
	mux.states[job.ID] = state

	go mux.runPlaceholder(job, state)
	waitPlaceholder(t, state)

	if len(ff.plainCalls) != 1 {
		t.Fatalf("expected plain placeholder call, got %d", len(ff.plainCalls))
	}
	if len(ff.imageCalls) != 0 {
		t.Fatalf("expected no image placeholder calls, got %d", len(ff.imageCalls))
	}
	cancel()
}

func TestRunPlaceholderUsesPlainWhenContentIDMissing(t *testing.T) {
	ff := &fakeMediaEngine{}
	mux := &Muxer{
		ffmpeg:          ff,
		placeholderPath: "placeholder.mp4",
		policy:          defaultPolicy(),
		states:          make(map[string]*playbackState),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	posterDir := t.TempDir()
	posterPath := filepath.Join(posterDir, posterFileName)
	_ = os.WriteFile(posterPath, []byte("poster"), 0644)

	job := &model.MuxJob{
		ID:          "job",
		ContentType: "",
		ContentID:   "",
	}

	state := &playbackState{
		ctx:           ctx,
		cancel:        cancel,
		cacheDir:      t.TempDir(),
		posterPath:    posterPath,
		lastRequested: -1,
		maxRequested:  -1,
	}
	mux.states[job.ID] = state

	go mux.runPlaceholder(job, state)
	waitPlaceholder(t, state)

	if len(ff.plainCalls) != 1 {
		t.Fatalf("expected plain placeholder call, got %d", len(ff.plainCalls))
	}
	if len(ff.imageCalls) != 0 {
		t.Fatalf("expected no image placeholder calls, got %d", len(ff.imageCalls))
	}
	cancel()
}

// TestStateForPreservesPosterPath verifies that stateFor captures the poster
// path from job.CacheDir before overwriting it with the playback cache dir.
func TestStateForPreservesPosterPath(t *testing.T) {
	mux := &Muxer{
		states: make(map[string]*playbackState),
	}

	posterDir := t.TempDir()
	posterPath := filepath.Join(posterDir, posterFileName)
	_ = os.WriteFile(posterPath, []byte("poster"), 0644)

	job := &model.MuxJob{
		ID:       "job1",
		CacheDir: posterDir,
	}

	state, err := mux.stateFor(job)
	if err != nil {
		t.Fatalf("stateFor: %v", err)
	}

	if state.posterPath != posterPath {
		t.Fatalf("posterPath = %q, want %q", state.posterPath, posterPath)
	}

	// job.CacheDir should now be the playback cache dir, not the poster dir.
	if job.CacheDir == posterDir {
		t.Fatal("job.CacheDir should have been overwritten")
	}
}