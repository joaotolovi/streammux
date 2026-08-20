package muxer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
		Background  string          `json:"background"`
		Logo        string          `json:"logo"`
		Runtime     string          `json:"runtime"`
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
	Title         string
	SeriesTitle   string
	Season        int
	Episode       int
	Year          string
	Rating        string
	LogoURL       string
	BackgroundURL string
	Duration      float64
}

func fetchCinemetaMetadata(ctx context.Context, client *http.Client, contentType, contentID string, languages ...string) (contentMetadata, error) {
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

	metadataURL := fmt.Sprintf("%s/meta/%s/%s.json", strings.TrimSuffix(cinemetaBaseURL, "/"), contentType, cinemetaID)
	if len(languages) > 0 {
		if language := cinemetaLanguageCode(languages[0]); language != "" {
			metadataURL += "?language=" + url.QueryEscape(language)
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, cinemetaMetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return contentMetadata{}, err
	}
	req.Header.Set("Accept", "application/json")
	if len(languages) > 0 {
		req.Header.Set("Accept-Language", cinemetaAcceptLanguage(languages[0]))
	}
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

	seriesTitle := strings.TrimSpace(meta.Meta.Name)
	title := seriesTitle
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
		Title:         title,
		SeriesTitle:   seriesTitle,
		Season:        season,
		Episode:       episode,
		Year:          firstReleaseYear(meta.Meta.ReleaseInfo),
		Rating:        rating,
		LogoURL:       normalizePosterURL(meta.Meta.Logo),
		BackgroundURL: normalizePosterURL(meta.Meta.Background),
		Duration:      parseCinemetaRuntime(meta.Meta.Runtime),
	}, nil
}

func parseCinemetaRuntime(raw string) float64 {
	var minutes int
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d min", &minutes); err == nil && minutes > 0 {
		return float64(minutes * 60)
	}
	return 0
}

func cinemetaAcceptLanguage(language string) string {
	code := cinemetaLanguageCode(language)
	if code == "" {
		return "en"
	}
	return code + ",en;q=0.8"
}

func cinemetaLanguageCode(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "portuguese (brazil)", "portuguese brazil", "pt-br", "pt_br":
		return "pt-BR"
	case "portuguese", "pt":
		return "pt"
	case "english", "en":
		return "en"
	case "spanish", "es":
		return "es"
	case "french", "fr":
		return "fr"
	case "german", "de":
		return "de"
	case "italian", "it":
		return "it"
	case "russian", "ru":
		return "ru"
	case "japanese", "ja":
		return "ja"
	case "korean", "ko":
		return "ko"
	default:
		language = strings.TrimSpace(language)
		if len(language) == 2 || (len(language) == 5 && language[2] == '-') {
			return language
		}
		return ""
	}
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

// fetchPoster downloads the film logo for the given Stremio content type and
// id to destPath. It uses the provided HTTP client with a short timeout so it
// never blocks the caller for long. Errors are swallowed; the caller should
// simply check whether the file was created.
func fetchPoster(ctx context.Context, client *http.Client, contentType, contentID, destPath string) error {
	metadata, err := fetchCinemetaMetadata(ctx, client, contentType, contentID)
	if err != nil {
		return err
	}
	logoURL := metadata.LogoURL
	if logoURL == "" {
		return fmt.Errorf("cinemeta response has no logo")
	}
	return downloadPoster(ctx, client, logoURL, destPath)
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
	return m.prefetchImagesForContent(ctx, contentType, contentID, destPath, "")
}

func (m *Muxer) prefetchImagesForContent(ctx context.Context, contentType, contentID, logoPath, backgroundPath string) chan struct{} {
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
		if logoPath != "" && metadata.LogoURL != "" {
			if err := downloadPoster(prefetchCtx, m.httpClient, metadata.LogoURL, logoPath); err != nil {
				log.Printf("mux: logo prefetch failed: %v", err)
			}
		}
		if backgroundPath != "" && metadata.BackgroundURL != "" {
			if err := downloadPoster(prefetchCtx, m.httpClient, metadata.BackgroundURL, backgroundPath); err != nil {
				log.Printf("mux: background poster prefetch failed: %v", err)
			}
		}
		log.Printf("mux: image prefetch completed")
	}()
	return done
}
