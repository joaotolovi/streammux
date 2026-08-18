package muxer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchPosterDownloadsImage(t *testing.T) {
	posterData := []byte("fake-poster-image")
	posterCalls := 0

	posterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posterCalls++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(posterData)
	}))
	defer posterServer.Close()

	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"poster":"` + posterServer.URL + `/poster.png"}}`))
	}))
	defer metaServer.Close()

	oldBase := cinemetaBaseURL
	cinemetaBaseURL = metaServer.URL
	defer func() { cinemetaBaseURL = oldBase }()

	dir := t.TempDir()
	dest := filepath.Join(dir, "poster.jpg")

	if err := fetchPoster(context.Background(), nil, "movie", "tt0111161", dest); err != nil {
		t.Fatalf("fetchPoster() error = %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("poster file not written: %v", err)
	}
	if string(got) != string(posterData) {
		t.Fatalf("poster data mismatch")
	}
	if posterCalls == 0 {
		t.Error("poster endpoint was never called")
	}
}

func TestFetchPosterRequiresContentTypeAndID(t *testing.T) {
	if err := fetchPoster(context.Background(), nil, "", "tt0111161", "/tmp/poster.jpg"); err == nil {
		t.Error("expected error for missing content type")
	}
	if err := fetchPoster(context.Background(), nil, "movie", "", "/tmp/poster.jpg"); err == nil {
		t.Error("expected error for missing content id")
	}
}
