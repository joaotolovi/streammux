package muxer

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

func interstitialDir(state *playbackState) string {
	if state == nil || state.cacheDir == "" {
		return ""
	}
	return filepath.Join(state.cacheDir, "interstitial")
}

func interstitialPlaylistPath(state *playbackState) string {
	dir := interstitialDir(state)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "intro.m3u8")
}

func interstitialAvailable(state *playbackState) bool {
	if state == nil {
		return false
	}
	state.mu.Lock()
	ready := state.interstitialReady
	state.mu.Unlock()
	if ready {
		return fileExists(interstitialPlaylistPath(state))
	}
	return fileExists(interstitialPlaylistPath(state))
}

func (m *Muxer) interstitialAssetURI(jobID string) string {
	path := "/mux/" + jobID + "/interstitial/intro.m3u8"
	if m.baseURL == "" {
		return ""
	}
	return m.baseURL + path
}

func (m *Muxer) ensureInterstitial(jobID string) {
	state := m.lookupState(jobID)
	if state == nil || m.placeholderPath == "" {
		return
	}
	state.mu.Lock()
	if state.interstitialReady || state.interstitialGenerating {
		state.mu.Unlock()
		return
	}
	state.interstitialGenerating = true
	state.mu.Unlock()

	go func() {
		dir := interstitialDir(state)
		if dir == "" {
			state.mu.Lock()
			state.interstitialGenerating = false
			state.mu.Unlock()
			return
		}
		// If already exists, mark ready and return.
		if fileExists(filepath.Join(dir, "intro.m3u8")) {
			state.mu.Lock()
			state.interstitialReady = true
			state.interstitialGenerating = false
			state.mu.Unlock()
			return
		}
		duration := m.policy.PlaceholderMinTime
		if duration <= 0 {
			duration = 8 * time.Second
		}
		// Cap to 10s to keep interstitial short even if PlaceholderMinTime larger.
		if duration > 10*time.Second {
			duration = 10 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := m.ffmpegGenerateInterstitial(ctx, m.placeholderPath, dir, duration)
		state.mu.Lock()
		state.interstitialGenerating = false
		if err == nil && fileExists(filepath.Join(dir, "intro.m3u8")) {
			state.interstitialReady = true
			log.Printf("mux: interstitial ready at %s (%.0fs)", dir, duration.Seconds())
		} else if err != nil {
			log.Printf("mux: interstitial generation failed: %v", err)
		}
		state.mu.Unlock()
	}()
}

// ffmpegGenerateInterstitial abstracts the ffmpeg call for testability.
func (m *Muxer) ffmpegGenerateInterstitial(ctx context.Context, placeholderPath, outputDir string, duration time.Duration) error {
	if gen, ok := m.ffmpeg.(interface {
		GenerateInterstitial(context.Context, string, string, time.Duration) error
	}); ok {
		return gen.GenerateInterstitial(ctx, placeholderPath, outputDir, duration)
	}
	// Fallback: try to generate via placeholder session if interface not implemented (tests)
	return nil
}

func (m *Muxer) interstitialPlaylist(jobID string) ([]byte, bool) {
	state := m.lookupState(jobID)
	if state == nil {
		return nil, false
	}
	path := interstitialPlaylistPath(state)
	if path == "" || !fileExists(path) {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

func (m *Muxer) InterstitialFilePath(jobID, file string) string {
	state := m.lookupState(jobID)
	if state == nil {
		return ""
	}
	dir := interstitialDir(state)
	if dir == "" {
		return ""
	}
	clean := filepath.Base(file)
	if clean == "." || clean == "/" || clean == "" {
		return ""
	}
	if clean == "intro.m3u8" {
		return filepath.Join(dir, clean)
	}
	var idx int
	if _, err := fmt.Sscanf(clean, "seg_%05d.ts", &idx); err != nil {
		return ""
	}
	path := filepath.Join(dir, clean)
	if !fileExists(path) {
		return ""
	}
	return path
}
