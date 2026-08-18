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

// cinemetaMeta is the subset of the Cinemeta meta response we need.
type cinemetaMeta struct {
	Meta struct {
		Poster string `json:"poster"`
	} `json:"meta"`
}

// fetchPoster downloads the film poster for the given Stremio content type and
// id to destPath. It uses the provided HTTP client with a short timeout so it
// never blocks the caller for long. Errors are swallowed; the caller should
// simply check whether the file was created.
func fetchPoster(ctx context.Context, client *http.Client, contentType, contentID, destPath string) error {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	if contentType == "" || contentID == "" {
		return fmt.Errorf("missing content type or id")
	}

	url := fmt.Sprintf("%s/meta/%s/%s.json", cinemetaBaseURL, contentType, contentID)

	reqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cinemeta returned status %d", resp.StatusCode)
	}

	var meta cinemetaMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return err
	}

	posterURL := meta.Meta.Poster
	if posterURL == "" {
		return fmt.Errorf("cinemeta response has no poster")
	}

	// Prefer the highest quality available.
	posterURL = strings.Replace(posterURL, "/small/", "/large/", 1)
	posterURL = strings.Replace(posterURL, "/medium/", "/large/", 1)

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
func (m *Muxer) prefetchPoster(ctx context.Context, contentType, contentID, destPath string) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		prefetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		if err := fetchPoster(prefetchCtx, m.httpClient, contentType, contentID, destPath); err != nil {
			log.Printf("mux: poster prefetch failed: %v", err)
		}
	}()
	return done
}
