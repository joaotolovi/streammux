package muxer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var cinemetaBaseURL = "https://v3-cinemeta.strem.io"

const cinemetaMetadataTimeout = 2 * time.Second

// cinemetaMeta is the subset of the Cinemeta meta response we need. Keep the
// fields deliberately small: the stream card needs identity and rating data,
// while source quality/language details still come from the addons.
type cinemetaMeta struct {
	Meta struct {
		Name        string          `json:"name"`
		Poster      string          `json:"poster"`
		ReleaseInfo string          `json:"releaseInfo"`
		IMDBRating  json.RawMessage `json:"imdbRating"`
		Videos      []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Season  int    `json:"season"`
			Episode int    `json:"episode"`
		} `json:"videos"`
	} `json:"meta"`
}

type contentMetadata struct {
	Title     string
	Year      string
	Rating    string
	PosterURL string
}

func fetchCinemetaMetadata(ctx context.Context, client *http.Client, contentType, contentID string) (contentMetadata, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if contentType == "" || contentID == "" {
		return contentMetadata{}, fmt.Errorf("missing content type or id")
	}

	cinemetaID := contentID
	season, episode := 0, 0
	if contentType == "series" {
		parts := strings.Split(contentID, ":")
		if len(parts) > 0 {
			cinemetaID = parts[0]
		}
		if len(parts) > 2 {
			_, _ = fmt.Sscanf(parts[1], "%d", &season)
			_, _ = fmt.Sscanf(parts[2], "%d", &episode)
		}
	}

	url := fmt.Sprintf("%s/meta/%s/%s.json", strings.TrimSuffix(cinemetaBaseURL, "/"), contentType, cinemetaID)
	reqCtx, cancel := context.WithTimeout(ctx, cinemetaMetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return contentMetadata{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return contentMetadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return contentMetadata{}, fmt.Errorf("cinemeta returned status %d", resp.StatusCode)
	}

	var meta cinemetaMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return contentMetadata{}, err
	}

	title := strings.TrimSpace(meta.Meta.Name)
	if season > 0 && episode > 0 {
		for _, video := range meta.Meta.Videos {
			if video.Season == season && video.Episode == episode {
				if name := strings.TrimSpace(video.Name); name != "" {
					title = name
				}
				break
			}
		}
	}

	rating := strings.TrimSpace(string(meta.Meta.IMDBRating))
	rating = strings.Trim(rating, `"`)
	if rating == "null" {
		rating = ""
	}
	return contentMetadata{
		Title:     title,
		Year:      firstReleaseYear(meta.Meta.ReleaseInfo),
		Rating:    rating,
		PosterURL: normalizePosterURL(meta.Meta.Poster),
	}, nil
}

func firstReleaseYear(releaseInfo string) string {
	for i := 0; i+4 <= len(releaseInfo); i++ {
		part := releaseInfo[i : i+4]
		valid := true
		for _, r := range part {
			if r < '0' || r > '9' {
				valid = false
				break
			}
		}
		if valid {
			return part
		}
	}
	return ""
}

func normalizePosterURL(posterURL string) string {
	posterURL = strings.TrimSpace(posterURL)
	posterURL = strings.Replace(posterURL, "/small/", "/large/", 1)
	posterURL = strings.Replace(posterURL, "/medium/", "/large/", 1)
	return posterURL
}

// fetchPoster downloads the film poster for the given Stremio content type and
// id to destPath. It uses the provided HTTP client with a short timeout so it
// never blocks the caller for long. Errors are swallowed; the caller should
// simply check whether the file was created.
func fetchPoster(ctx context.Context, client *http.Client, contentType, contentID, destPath string) error {
	metadata, err := fetchCinemetaMetadata(ctx, client, contentType, contentID)
	if err != nil {
		return err
	}
	posterURL := metadata.PosterURL
	if posterURL == "" {
		return fmt.Errorf("cinemeta response has no poster")
	}
	return downloadPoster(ctx, client, posterURL, destPath)
}

func downloadPoster(ctx context.Context, client *http.Client, posterURL, destPath string) error {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	imgCtx, imgCancel := context.WithTimeout(ctx, 5*time.Second)
	defer imgCancel()

	imgReq, err := http.NewRequestWithContext(imgCtx, http.MethodGet, posterURL, nil)
	if err != nil {
		return err
	}

	imgResp, err := client.Do(imgReq)
	if err != nil {
		return err
	}
	defer imgResp.Body.Close()

	if imgResp.StatusCode != http.StatusOK {
		return fmt.Errorf("poster image returned status %d", imgResp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}

	tmpPath := destPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, imgResp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return err
	}
	return nil
}

// prefetchPoster starts a best-effort poster download in the background. The
// returned channel is closed once the attempt finishes (success or failure).
// It deliberately uses context.Background() instead of the caller's context
// because the caller (Process) runs inside an HTTP request that completes as
// soon as the stream list is returned — using the request context would cancel
// the download before it finishes.
func (m *Muxer) prefetchPoster(_ context.Context, contentType, contentID, destPath string) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		prefetchCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := fetchPoster(prefetchCtx, m.httpClient, contentType, contentID, destPath); err != nil {
			log.Printf("mux: poster prefetch failed: %v", err)
		} else {
			log.Printf("mux: poster prefetch ok -> %s", destPath)
		}
	}()
	return done
}

func (m *Muxer) prefetchPosterURL(ctx context.Context, posterURL, destPath string) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		prefetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		if err := downloadPoster(prefetchCtx, m.httpClient, posterURL, destPath); err != nil {
			log.Printf("mux: poster prefetch failed: %v", err)
			return
		}
		log.Printf("mux: poster prefetch ok -> %s", destPath)
	}()
	return done
}

func (m *Muxer) prefetchPosterForContent(ctx context.Context, contentType, contentID, destPath string) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		prefetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		metadata, err := fetchCinemetaMetadata(prefetchCtx, m.httpClient, contentType, contentID)
		if err != nil {
			log.Printf("mux: background Cinemeta metadata failed: %v", err)
			return
		}
		if metadata.PosterURL == "" {
			return
		}
		if err := downloadPoster(prefetchCtx, m.httpClient, metadata.PosterURL, destPath); err != nil {
			log.Printf("mux: poster prefetch failed: %v", err)
			return
		}
		log.Printf("mux: poster prefetch ok -> %s", destPath)
	}()
	return done
}
