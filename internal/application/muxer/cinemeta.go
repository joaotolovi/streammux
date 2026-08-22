package muxer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	Title       string
	SeriesTitle string
	Season      int
	Episode     int
	Year        string
	Rating      string
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
		Title:       title,
		SeriesTitle: seriesTitle,
		Season:      season,
		Episode:     episode,
		Year:        firstReleaseYear(meta.Meta.ReleaseInfo),
		Rating:      rating,
	}, nil
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
