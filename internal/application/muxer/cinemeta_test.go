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

func TestFetchPosterStripsEpisodeSuffixForSeries(t *testing.T) {
	var receivedPath string
	posterData := []byte("fake-poster")

	posterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(posterData)
	}))
	defer posterServer.Close()

	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"poster":"` + posterServer.URL + `/poster.jpg"}}`))
	}))
	defer metaServer.Close()

	oldBase := cinemetaBaseURL
	cinemetaBaseURL = metaServer.URL
	defer func() { cinemetaBaseURL = oldBase }()

	dir := t.TempDir()
	dest := filepath.Join(dir, "poster.jpg")

	// Use a series episode ID with :season:episode suffix.
	if err := fetchPoster(context.Background(), metaServer.Client(), "series", "tt14681924:1:1", dest); err != nil {
		t.Fatalf("fetchPoster() error = %v", err)
	}

	// The Cinemeta URL should use just the IMDB ID without the episode suffix.
	if receivedPath != "/meta/series/tt14681924.json" {
		t.Fatalf("expected /meta/series/tt14681924.json, got %s", receivedPath)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("poster file not written: %v", err)
	}
	if string(got) != string(posterData) {
		t.Fatalf("poster data mismatch")
	}
}

func TestFetchCinemetaMetadataExtractsPresentationFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"name":"Oppenheimer","releaseInfo":"2023-2024","imdbRating":8.6,"videos":[{"season":1,"episode":1,"name":"Pilot"}]}}`))
	}))
	defer server.Close()

	oldBase := cinemetaBaseURL
	cinemetaBaseURL = server.URL
	defer func() { cinemetaBaseURL = oldBase }()

	metadata, err := fetchCinemetaMetadata(context.Background(), server.Client(), "movie", "tt123")
	if err != nil {
		t.Fatalf("fetchCinemetaMetadata() error = %v", err)
	}
	if metadata.Title != "Oppenheimer" || metadata.Year != "2023" || metadata.Rating != "8.6" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}

	metadata, err = fetchCinemetaMetadata(context.Background(), server.Client(), "series", "tt123:1:1")
	if err != nil {
		t.Fatalf("series metadata error = %v", err)
	}
	if metadata.Title != "Pilot" {
		t.Fatalf("episode title = %q, want Pilot", metadata.Title)
	}
}
