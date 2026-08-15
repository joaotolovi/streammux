package model

import "github.com/streammux/streammux/internal/domain/constants"

type Service struct {
	ID          string            `json:"id"`
	Enabled     bool              `json:"enabled"`
	Credentials map[string]string `json:"credentials"`
}

type Addon struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ManifestURL string `json:"manifestUrl"`
	Role        string `json:"role"`
	Language    string `json:"language"`
	Enabled     bool   `json:"enabled"`
	Timeout     int    `json:"timeout,omitempty"`
}

type Config struct {
	Language string    `json:"language"`
	Services []Service `json:"services"`
	Addons   []Addon   `json:"addons"`
}

type User struct {
	UUID              string `json:"uuid"`
	EncryptedPassword string `json:"encryptedPassword"`
	Config            Config `json:"config"`
}

type ParsedFile struct {
	Resolution    string   `json:"resolution"`
	Quality       string   `json:"quality"`
	Encode        string   `json:"encode"`
	VisualTags    []string `json:"visualTags"`
	AudioTags     []string `json:"audioTags"`
	AudioChannels []string `json:"audioChannels"`
	Languages     []string `json:"languages"`
	ReleaseGroup  string   `json:"releaseGroup"`
}

type CollectedStream struct {
	AddonID       string     `json:"addonId"`
	AddonName     string     `json:"addonName"`
	AddonRole     string     `json:"addonRole"`
	AddonLanguage string     `json:"addonLanguage"`
	Stream        Stream     `json:"stream"`
	Parsed        ParsedFile `json:"parsed"`
	Size          int64      `json:"size"`
	IsDubbed      bool       `json:"isDubbed"`
	Language      string     `json:"language"`
}

type Stream struct {
	Name          string         `json:"name"`
	Title         string         `json:"title,omitempty"`
	Description   string         `json:"description,omitempty"`
	URL           string         `json:"url"`
	InfoHash      string         `json:"infoHash,omitempty"`
	FileIdx       *int           `json:"fileIdx,omitempty"`
	Size          int64          `json:"size,omitempty"`
	BehaviorHints map[string]any `json:"behaviorHints,omitempty"`
}

type StremioStream struct {
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	URL           string         `json:"url,omitempty"`
	InfoHash      string         `json:"infoHash,omitempty"`
	FileIdx       *int           `json:"fileIdx,omitempty"`
	BehaviorHints map[string]any `json:"behaviorHints,omitempty"`
}

type MuxJob struct {
	ID             string `json:"id"`
	VideoURL       string `json:"videoUrl"`
	AudioURL       string `json:"audioUrl"`
	TargetLanguage string `json:"targetLanguage"`
	Title          string `json:"title"`

	// Runtime fields (not serialized):
	Duration      float64 `json:"-"` // probed once, cached
	CacheDir      string  `json:"-"` // temp dir for cached segments
	PlaylistReady bool    `json:"-"` // playlist has been written

	// AudioTrackIndex is the numeric index of the target-language audio track
	// (resolved once by probing the audio source). -1 when unknown.
	AudioTrackIndex int `json:"-"`

	// Resolved URLs (not serialized). Addon URLs redirect through a debrid
	// proxy (e.g. torrentio → torbox API → CDN) that is slow to re-resolve on
	// every request. We resolve once and use the final CDN URL directly, which
	// supports HTTP Range and answers in milliseconds.
	VideoResolved string `json:"-"`
	AudioResolved string `json:"-"`

	// AudioCandidates (not serialized) is the ordered list of audio source URLs
	// to try, best first. Debrid sources sometimes return a short error video
	// (no audio track) instead of the real file; we fall back through this
	// list until one yields a usable audio track.
	AudioCandidates []string `json:"-"`

	// VideoCandidates (not serialized) is the ordered list of video source URLs
	// to try, best first, excluding the primary. Used when the primary video is
	// a broken debrid response (e.g. a short trailer instead of the movie).
	VideoCandidates []string `json:"-"`
}

type Manifest struct {
	ID            string            `json:"id"`
	Version       string            `json:"version"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Types         []string          `json:"types"`
	Resources     []string          `json:"resources"`
	Logo          string            `json:"logo,omitempty"`
	Catalogs      []ManifestCatalog `json:"catalogs,omitempty"`
	BehaviorHints map[string]any    `json:"behaviorHints,omitempty"`
}

type ManifestCatalog struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	ExtraTypes []string `json:"extraTypes,omitempty"`
}

func (c *Config) ValidAddons() []Addon {
	var out []Addon
	for _, a := range c.Addons {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

func (c *Config) ValidServices() []Service {
	var out []Service
	for _, s := range c.Services {
		if s.Enabled && len(s.Credentials) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) AddonsByRole(role string) []Addon {
	var out []Addon
	for _, a := range c.ValidAddons() {
		if a.Role == role || a.Role == constants.RoleBoth {
			out = append(out, a)
		}
	}
	return out
}
